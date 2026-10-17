# Fork architecture

Last verified: 2026-10-17, on commit `6658ee1d` of `fork/release` plus the final review fix wave
committed on top of it in the same branch. Base: upstream `kimdre/doco-cd` `v0.127.0`
(`772ed15`). Binding design: the Portico spec
`docs/superpowers/specs/2026-10-15-portico-v2-doco-cd-fork-design.md` in
`lucapolignone/portico`, with the errata of 2026-10-17. This file describes the product (layers,
flows, invariants, known debts); release and merge process live in the workflows and in the
README. The upstream documentation is in `wiki/`.

## Layers

Five new packages, named as upstream would name them so that each can be proposed upstream:

| Package | Role |
|---|---|
| `internal/auth/jwt` | JWT verification against a JWKS (`RS256`, `ES256`), cached, refreshed on an unknown `kid` at most every 30 s |
| `internal/rbac` | Argo CD-style CSV policy, `Enforcer` with reload on mtime/size, `Principal` carried in the context |
| `internal/composepolicy` | Policy file, `Holder` with reload, `Check` on the resolved compose project, path rules |
| `internal/rollback` | Failure store and last good under `<data>/fork/`, `Run` (pre_rollback, then redeploy), image pinning |
| `internal/hooks` | Signing, `pre_rollback` client, outcome events and their outbox |

Smaller fork files in upstream packages: `internal/filesystem/atomic_write.go`,
`internal/config/deploy/{object,rollback}.go`, `internal/git/fork_lastgood_ref.go`,
`internal/docker/fork_job_gate.go`, `internal/controlplane/fork_poll.go`,
`internal/restapi/auth.go`, `internal/api/fork_{auth,unblock}.go`, `internal/mcp/fork_auth.go`,
`internal/stages/fork_{policy,rollback,outcome}.go`.

**Registries, nil until a key is set.** `cmd/doco-cd/main.go` fills them at startup, before any
scheduler or handler runs; a nil registry is the upstream behaviour.

| Registry | Set when | Effect when nil |
|---|---|---|
| `stages.SetComposePolicy` | `COMPOSE_POLICY_FILE` | no policy check, no job gate |
| `docker.ForkJobGate` | installed by `SetComposePolicy` | jobs run as upstream |
| `stages.SetRBAC` | `RBAC_POLICY_FILE` | `Enforce` allows everything, lists are not filtered |
| `stages.SetPreRollback` | `HOOK_PRE_ROLLBACK_URL` | every rollback decides `unconfigured`, hence `blocked` |
| `stages.SetOutbox` | `HOOK_OUTCOME_URL` | no event, no `<data>/fork/outbox`, no outcome metrics |

The API and MCP handlers receive the `restapi.Authenticator` and the enforcer through their
`Dependencies` (`Auth`, `RBAC`); without JWT the authenticator is the upstream `x-api-key` check.
Per-target rollback is the deploy config key `rollback` (`deploy.RollbackConfig`, a pointer with
`omitempty`), not an app key.

**Upstream touches.** Each one is a few lines marked `// Fork:` that call into the fork files:

- `internal/stages/run.go`: stage functions of the deploy and destroy orders, target lock
  release, last good and success outcome before/after `clearDeploymentFailure`, rollback and
  failure outcome in `handleStageFailure`, disk failure in `recordDeploymentFailure` and
  `lastDeploymentFailure`;
- `internal/stages/stage_2_pre-deploy.go`: poison skip after `lastDeploymentFailure`,
  `checkForkPolicies` right after `docker.LoadCompose` in `loadComposeProjectHash`;
- `internal/stages/stage_1_init.go` (declared files, `newGitStore`),
  `stage_3_deploy.go` (deployed commit), `skip.go` (`SkipReasonRolledBack`), `types.go` (fields);
- `internal/docker/compose_scheduled_run.go`: `checkForkJobGate` at the top of
  `loadComposeScheduledProjectAll`;
- `internal/api/*.go` and `internal/mcp/*.go`: `ValidateApiKey` replaced by
  `authenticate`/`authorize`, list filtering, the MCP per-tool authorization middleware;
- `internal/controlplane/poll.go`: the `api` trigger marker;
- `internal/config/app/app_config.go`: the new keys and their startup validation;
  `internal/config/deploy/deploy.go`: the `Rollback` field;
- `cmd/doco-cd/{main,apiserver,selfupdate_report,version}.go`: wiring, self-update outcome,
  version check against the base version.

## Flows

**Authentication and authorization.** Every REST handler calls `authenticate` where upstream
called `ValidateApiKey`: with JWT on, a present `Authorization` header decides alone (it must be
`Bearer <jwt>`), otherwise the `x-api-key` authenticates the fixed subject `apikey` (if
`API_KEY_ENABLED`). The 401 body and message are upstream's. `authorize(principal, resource,
action, object)` runs as soon as the object `<context>/<project>` is known (after the Docker
context is resolved) and answers 403. Lists are filtered by `get`. `poll/run` and MCP
`trigger_poll` accept only entries of the operator's poll config (`RestrictPollConfigs`) and
carry the principal into the run, where pre-deploy checks `targets, sync` per project. MCP
authenticates the connection and authorizes each tool call; with RBAC on an unclassified tool is
denied. The enforcer writes the audit line of every deny and of every allow on a mutating action.

**Pre-deploy order** (`runForkPreDeployStage`):

1. `targets, sync` for a run that carries a principal, before anything else: a deny is a
   terminal `PolicyDeniedError` of the target and does not touch the job gate record;
2. upstream `RunPreDeployStage`: external secrets are resolved first, then the failure is read
   (memory, else disk);
3. poison skip (`rollbackPoisoned`): a commit that ended `rolled_back`, `blocked` or
   `rollback_failed` is skipped with `SkipReasonRolledBack`; a new commit resets the record to
   `failed` and emits `unblocked`. After a restart the skipped target gets its job gate verdict
   from the project that runs (`verifyPoisonedTarget`, see «Invariants»);
4. upstream skip checks, then `loadComposeProjectHash`, where `checkForkPolicies` applies the
   Swarm rule and the compose policy to the resolved project and records the verdict for the job
   gate;
5. `forkPolicyGate`: on a first deploy that upstream does not load in pre-deploy, the project is
   loaded so that step 4 still runs.

A policy violation is terminal, records no failure (pre-deploy), starts no rollback and emits
`policy_denied` once per (object, commit, violations). A denial whose only violation is
`targets/sync` concerns the caller: it emits `policy_denied` and keeps the audit line, but sends
no failure notification and posts no failed commit status on the target owner's commit.

**Deploy, lock, rollback, outbox.** For a target with rollback enabled, `runForkDeployStage`
takes the per-object fork target lock, checks the poison again under it (a job queued behind a
rollback skips the commit that rollback marked), and holds the lock until `RunStages` returns.
On success `recordLastGood` writes `lastgood/<ctx>/<project>.json` (commit, source URL, deploy
config with `Internal` zeroed, image IDs and digests) and the `refs/doco-cd/lastgood/...` ref,
then the failure is cleared and `deployed` is emitted. On a failure of deploy, post-deploy or
cleanup, `handleStageFailure` persists the failure (`failed`), then `rollback.Run`: no last good
is `failed`; an unsupported source (Swarm, OCI, `repository_url`) is `rollback_failed`; else
`pre_rollback` is asked and anything but a 2xx `proceed` is `blocked`; on `proceed` the last
good commit is republished from the mirror, rendered with the saved config and the secrets of
now, its images pinned, the compose policy in force applied, and deployed with a full force
recreate within `rollback.timeout`. The outcome event goes to the outbox: one file per event,
written atomically, delivered at least once by one worker, FIFO per object, retried with
backoff for up to 24 h, then `dead/`. `POST /project/{p}/unblock` (REST only, and only with
`HOOK_PRE_ROLLBACK_URL`) puts the record back to `failed` with the same commit under the target
lock.

**Self-update handover.** When Doco-CD deploys itself, the predecessor's stage returns
`selfupdate.ErrHandover`: `handleStageFailure` returns first, with no failure, no rollback and
no event. The successor resolves the journal and `selfUpdateReporter` emits `deployed` once
healthy, or `failed` when the handover did not become healthy.

## Invariants

- **No keys, no fork.** Without the new keys there is no `<data>/fork/`, no extra Prometheus
  metric (the outcome metrics register in `hooks.NewOutbox`), the same routes, the same 401 and
  the same OpenAPI document except the additive deploy config `rollback` property and the
  `RollbackConfig` component (`internal/api/fork_openapi_test.go` compares canonical JSON with
  the boolean defaults normalized). `deploy.Config.Hash()` is unchanged without `rollback`.
  Without `rollback.enabled` a target writes nothing under `<data>/fork/`.
- **Target identity** is `<context>/<project>` (`deploy.ObjectName`, empty context written
  `default`); it keys RBAC objects, the store, the lock and the events. In globs `*` does not
  cross `/`. With a compose policy an object is accepted only if the `targets` entry binds it to
  the repository that holds the deploy config.
- **Lock order.** Deployment limiters (pre-deploy and mutation admission) → fork target lock →
  `lock.LockStack` (inside `DeployStack`). The holder of the target lock never waits for a
  limiter and nothing takes the target lock while holding `LockStack`. A job that gives up
  waiting for the target lock records nothing.
- **Redaction before truncation.** The error of a failure is redacted of the
  `Internal.Environment` values (raw, `%q` and JSON forms) and only then truncated to 2000 runes,
  on every fork path: failure store, `pre_rollback`, outcome events.
- **No redirects on signed hooks.** The `pre_rollback` and outbox clients are copies with
  `CheckRedirect` returning `http.ErrUseLastResponse`: a signed body is never re-posted, and an
  unsigned redirected answer is never accepted.
- **Job gate.** With a compose policy every job that rebuilds containers from a deployed revision
  runs only on an object whose last check in this process passed. After a restart the check of a
  poisoned target is made on what runs: the failed commit of a `blocked` target, the last good
  of a `rolled_back` one, against the policy in force; a `rollback_failed` target gets no verdict
  and its jobs stay refused until a new commit or an unblock.
- **Never a blind deploy.** An unreadable failure record counts as `blocked`; a corrupt one is
  replaced by a `blocked` record of the revision seen, which a new commit releases.
- **Bounded rollback.** `rollback.timeout` is at most 30 m and `pre_rollback_timeout` at most
  5 m (defaults 5 m and 30 s): both are held under the target lock and a mutation slot.

## Formats

- **RBAC** (`RBAC_POLICY_FILE`): Argo CD CSV, `p, <subject|role>, <resource>, <action>,
  <object>, <allow|deny>` (effect mandatory) and `g, <group|sub>, role:<name>`; deny by default,
  an explicit deny wins. Resources and actions per route and per MCP tool: spec, «Mappa».
- **Compose policy** (`COMPOSE_POLICY_FILE`): YAML `version: 1`, `default` and `targets`, each
  with `repository`, `reference`, `config_target`, `bind_allowlist`, `cap_add_allowlist`.
  `reference` matches like `git.ReferenceMatches`, so a tag is declared in full form
  `refs/tags/<tag>`. Rules: spec, «Compose policy».
- **Signature**: `X-Doco-Signature: t=<unix>,v1=<hex>`, HMAC-SHA256 with `HOOK_SECRET` over
  `<t>.<body>`, plus `X-Doco-Delivery: <uuid>`; both recomputed on every attempt. Receivers
  reject `t` older than 5 minutes and repeated deliveries, and deduplicate by `event_id`.
- **Outcome payload**: `doco-cd.outcome/v1` (`event_id`, `object`, `outcome`, `commit`,
  `trigger`, `stage`, `error`, `rollback`, `violations`, `images`, ...); fields that do not apply
  are omitted. `pre_rollback` request: `doco-cd.pre_rollback/v1`. Schemas: spec,
  «`hooks.outcome`» and «Flusso al fallimento».

## Documented behaviours

- `PASS_ENV=true` passes the Doco-CD environment to compose interpolation: with `HOOK_SECRET` in
  the environment an app can read it through `${HOOK_SECRET}`. Use `HOOK_SECRET_FILE`.
- Reconciliation jobs inherit the principal of the run that started them, as they inherit its
  `OriginManual`.
- With JWT on, any `Authorization` header that is not `Bearer` (e.g. Basic from a proxy) fails
  authentication even when a valid `x-api-key` is present.
- The compose policy applies at deploy time: a tightened policy does not re-check an unchanged
  stack until it changes, and its jobs keep the last verdict of this process.
- Unblock exists only on REST, not MCP. `API_KEY_ENABLED=false` without JWT or RBAC starts and
  closes the API.

## Known debts

- A symlink created by a container inside an rw allowlisted directory, pointing to the Docker
  socket, passes on the next deploy — https://app.notion.com/p/3fba27896a5381b0a41dd3db29816d7c
- PR 3 hardening (reconciliation cleanup without `targets/sync`, one deny audit line per hidden
  list item, 400 vs 403 reveals a context, JWT failure logged as invalid api key, OpenAPI
  without Bearer and 403) — https://app.notion.com/p/3fca27896a538134852adaf1835c2466
- PR 4 rollback limits (unblock 500 shows the store path, unblock of a corrupt record writes an
  empty commit, unblock waits up to both rollback timeouts; OCI targets are never rolled back and
  end `rollback_failed`; a `rollback_failed` target keeps its jobs refused after a restart; an
  unreadable, not corrupt, record stays blocked on every commit) —
  https://app.notion.com/p/3fca27896a5381b78d20ee5d794e8ef6
- PR 5 outcome limits (violations and the `rollback_failed` log line not redacted, two
  concurrent pre-deploys may emit two `unblocked`, repeated `failed` events, `dead/` without a
  cap, no redaction in the upstream sinks) —
  https://app.notion.com/p/3fca27896a5381e3abc2f0a910177734
- The upstream merge workflow needs `UPSTREAM_MERGE_TOKEN` —
  https://app.notion.com/p/3fca27896a538191b34cd06425995426
- The upstream OpenAPI generator is nondeterministic on boolean defaults —
  https://app.notion.com/p/3fca27896a5381f8bf74f220c689cd2b
- Object names are not case-folded; `apply-self` in a separate process has no job gate; the
  compose policy `label=` rule blocks only `spc_t` and `LoadCompose` `WithOsEnv` exposes the
  Doco-CD environment to `${...}`; two outboxes may run on the same directory during the
  self-update overlap; the number of upstream files touched should shrink —
  https://app.notion.com/p/3fca27896a5381a28ab7e22caa26d545
