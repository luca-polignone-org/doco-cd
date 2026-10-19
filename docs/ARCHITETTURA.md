# Fork architecture

Last verified: 2026-10-19, on commit `20f46cb3` of `main` plus the branch `fix/rollback-image-ref`
(TCK-644, rollback image references and the retry after a rollback); before that on 2026-10-17, on
commit `3e1fdcb2` (PR #11 merged, the last code PR of `v0.127.0-poli.2`) plus the e2e commit
`b26d341f` of `fork/release-poli2`. Base: upstream
`kimdre/doco-cd` `v0.127.0` (`772ed15`). Binding design: the Portico spec
`docs/superpowers/specs/2026-10-15-portico-v2-doco-cd-fork-design.md` in
`lucapolignone/portico`, with the errata of 2026-10-17, and for `poli.2` the specs 3c, 4 and 5 of
Portico v2 with the errata listed at the end of this file. This file describes the product (layers,
flows, invariants, known debts); release and merge process live in the workflows and in the
README. The upstream documentation is in `wiki/`.

## Layers

Five new packages, named as upstream would name them so that each can be proposed upstream:

| Package | Role |
|---|---|
| `internal/auth/jwt` | JWT verification against a JWKS (`RS256`, `ES256`), cached, refreshed on an unknown `kid` at most every 30 s |
| `internal/rbac` | Argo CD-style CSV policy, `Enforcer` with reload on mtime/size, `Principal` carried in the context |
| `internal/composepolicy` | Policy file, `Holder` with reload, `Check` on the resolved compose project, path rules; since `poli.2` also `CheckSecrets` (`secrets.go`) and the logging rule (`logging.go`) |
| `internal/rollback` | Failure store and last good under `<data>/fork/`, `Run` (pre_rollback, then redeploy), image pinning |
| `internal/hooks` | Signing, `pre_rollback` client, outcome events and their outbox |

Smaller fork files in upstream packages: `internal/filesystem/atomic_write.go`,
`internal/config/deploy/{object,rollback}.go`, `internal/git/fork_lastgood_ref.go`,
`internal/docker/fork_job_gate.go`, `internal/controlplane/fork_poll.go`,
`internal/restapi/auth.go`, `internal/api/fork_{auth,unblock}.go`, `internal/mcp/fork_auth.go`,
`internal/stages/fork_{policy,rollback,outcome}.go`. Added by `poli.2`:
`internal/docker/fork_secret_gate.go`, `internal/git/fork_content_key.go` (`TreeHashAt`),
`internal/secretprovider/openbao/fork_token_reload.go`,
`internal/stages/fork_{secrets,content_key}.go`. In the fork packages, `poli.2` also changes
`internal/composepolicy/{check,file}.go` (the `secret_allowlist` and `logging` keys, `checkLogging`
in `checkService`) and `internal/rollback/{store,lastgood}.go` (failure store version 2 with
`content_key`, last good still version 1).

**Registries, nil until a key is set.** `cmd/doco-cd/main.go` fills them at startup, before any
scheduler or handler runs; a nil registry is the upstream behaviour.

| Registry | Set when | Effect when nil |
|---|---|---|
| `stages.SetComposePolicy` | `COMPOSE_POLICY_FILE` | no policy check, no job gate |
| `docker.ForkJobGate` | installed by `SetComposePolicy` | jobs run as upstream |
| `docker.ForkSecretGate` | installed by `SetComposePolicy` | scheduled jobs resolve secrets as upstream |
| `stages.SetRBAC` | `RBAC_POLICY_FILE` | `Enforce` allows everything, lists are not filtered |
| `stages.SetPreRollback` | `HOOK_PRE_ROLLBACK_URL` | every rollback decides `unconfigured`, hence `blocked` |
| `stages.SetOutbox` | `HOOK_OUTCOME_URL` | no event, no `<data>/fork/outbox`, no outcome metrics |

The API and MCP handlers receive the `restapi.Authenticator` and the enforcer through their
`Dependencies` (`Auth`, `RBAC`); without JWT the authenticator is the upstream `x-api-key` check.
Per-target rollback is the deploy config key `rollback` (`deploy.RollbackConfig`, a pointer with
`omitempty`), not an app key.

**Upstream touches.** Each one is a few lines that call into the fork files, mostly marked
`// Fork:` (the OpenBao call sites wrapped by `retryOnForbidden` are not marked one by one):

- `internal/stages/run.go`: stage functions of the deploy and destroy orders, target lock
  release, last good and success outcome before/after `clearDeploymentFailure`, rollback and
  failure outcome in `handleStageFailure`, disk failure in `recordDeploymentFailure` and
  `lastDeploymentFailure`;
- `internal/stages/stage_2_pre-deploy.go`: poison skip after `lastDeploymentFailure`,
  `forkRetryRecreates` in the retry's force recreate,
  `checkForkPolicies` right after `docker.LoadCompose` in `loadComposeProjectHash`,
  `checkForkSecrets` right before `ResolveSecretReferences`;
- `internal/stages/stage_1_init.go` (declared files, `newGitStore`),
  `stage_3_deploy.go` (deployed commit), `skip.go` (`SkipReasonRolledBack`), `types.go` (fields);
- `internal/docker/compose_scheduled_run.go`: `checkForkJobGate` at the top of
  `loadComposeScheduledProjectAll`, which also puts the checked object in the context
  (`withForkJobGateObject`, only while `ForkSecretGate` is set), and `checkForkSecretGate` right
  before `ResolveSecretReferences` in `prepareComposeScheduledDeployConfig`;
- `internal/secretprovider/secretprovider.go`, `internal/secretprovider/openbao/{client,config}.go`:
  `EnableTokenReload`, the `SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD` key, `refreshToken` and
  `retryOnForbidden` around the OpenBao calls;
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
   `failed` and emits `unblocked`. The upstream retry of that `failed` record force recreates the
   project, except when the record carries `rolled_back_to` (`forkRetryRecreates`): the rollback
   already replaced the half-applied containers with the complete ones of the last good, so the
   retry is a regular deploy and a commit with the content of the last good recreates nothing. After a restart the skipped target gets its job gate verdict
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
now, its images pinned (`rollback.PinImages`: the reference of the last good's compose file when
it still resolves locally to the recorded image ID, so `docker ps`, the outcome images and the
compose config hash are those of a regular deploy of that content; else the image ID; else the
first registry digest; a local image gets `pull_policy: never`, a pinned service loses `build`),
the compose policy in force applied, and deployed with a full force recreate within
`rollback.timeout`. The compose hash it stamps is taken before the pin, so the labels describe the
last good's content as a regular deploy of it would. A `rolled_back` record names the last good it deployed in `rolled_back_to`
(store version 2, an `omitempty` field), kept when a new commit or unblock puts it back to
`failed`, dropped by the next recorded failure. The outcome event goes to the outbox: one file per event,
written atomically, delivered at least once by one worker, FIFO per object, retried with
backoff for up to 24 h, then `dead/`. `POST /project/{p}/unblock` (REST only, and only with
`HOOK_PRE_ROLLBACK_URL`) puts the record back to `failed` with the same commit under the target
lock.

**Self-update handover.** When Doco-CD deploys itself, the predecessor's stage returns
`selfupdate.ErrHandover`: `handleStageFailure` returns first, with no failure, no rollback and
no event. The successor resolves the journal and `selfUpdateReporter` emits `deployed` once
healthy, or `failed` when the handover did not become healthy.

**Secret allowlist (`poli.2`).** With a compose policy, the encoded references (the exact map the
provider receives, after `INTERPOLATE_EXTERNAL_SECRETS`) are checked by `CheckSecrets` right
before each `ResolveSecretReferences` of production code. First the identity: an object missing
from `targets`, or whose repository, reference or config target does not match, gets the
violation `target`, with or without `secret_allowlist`. Then, if `secret_allowlist` is set
(target, else `default`), every reference must match an entry. Three hooks, three outcomes:

| Hook | Where | Outcome of a denial |
|---|---|---|
| pre-deploy | `checkForkSecrets` in `RunPreDeployStage` | `policy_denied`, terminal; the object is marked denied for the job gate |
| scheduled jobs, cert rotation, self-apply | `checkForkSecretGate` → `docker.ForkSecretGate` (`forkSecretGate`) in `prepareComposeScheduledDeployConfig` | the job is refused; also refused when the context carries no job gate object or the reloaded deploy config names another object |
| rollback | `checkForkSecretsAs(lastGoodIdentity)` in `loadLastGoodFiles` | the rollback fails, `rollback_failed`; the job gate record is not touched |

An OpenBao `kv:` reference is split as upstream does and matches an entry
`kv:[<ns>:]<engine>:<prefix>` with the same namespace and engine and a name equal to the prefix
or below it by path segment; `pki:`/`pki-role:` match only a literal, full entry; other providers
compare a literal prefix.

**Logging rule (`poli.2`).** With a compose policy every service of every target is checked:
`services.*.logging` is allowed only if driver and options are equal to `default.logging`
(without `default.logging` any `logging` is denied), and a service label whose journald form
(`sanitizeKeyMod`) starts with `COM_DOCKER_COMPOSE_` is denied. The violations are ordinary
policy violations: `policy_denied` in pre-deploy, `rollback_failed` on the last good.

**Token reload (`poli.2`).** With `SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD=true` every public
method of the OpenBao provider stats the token file first (`refreshToken`, under a mutex) and
swaps the token of both clients when mtime or size changed. An empty or unreadable file keeps the
token in use and logs one ERROR per distinct failure, and one line on recovery. A 403 on an
idempotent read re-reads the file once and retries once (`retryOnForbidden`); `pki-role`
issuance is never retried.

**Poison by content (`poli.2`).** With `rollback.unblock_on: content`, the failure record carries
`content_key` = `tree:<git tree hash of working_dir at the failed commit, from the deploy
mirror>;config:<git blob hash of the deploy config file read from disk>`. In `rollbackPoisoned` a
new commit whose key equals the recorded one stays poisoned (skipped, with the job gate verdict of
`verifyPoisonedTarget`); a different key resets the record to `failed` and emits `unblocked`, as
in commit mode. Fail closed: a key that cannot be computed for the new commit (unreadable mirror,
`working_dir` gone, file unreadable) counts as the same content and the target stays poisoned. A
key that cannot be computed when the failure is recorded is left empty, with one Warn per failure,
and that record releases on a new commit as in commit mode: this is the case of OCI sources and of
deploy configs without `Internal.File` (inline, auto-discovery).

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
- **Every secret read is gated.** Each `ResolveSecretReferences` (and `GetSecret`/`GetSecrets`)
  of production code under `internal/` and `cmd/`, outside `internal/secretprovider`, is
  preceded by its allowlist check;
  `TestEveryResolveIsGated` (`internal/stages/fork_secret_allowlist_test.go`) walks the sources
  and wants exactly the three known call sites, so an upstream merge that adds a read breaks it.
- **No secret in the logs.** Neither a resolved value, nor the token, nor the content of the token
  file appear in logs, errors, outcomes or on disk. A reference is not a secret and may appear.
- **`poli.2` without keys is `poli.1`**, except two incompatible changes that hold with any compose
  policy and have no key to turn them off: the logging rule (errata 4), and the `target` denial
  (`policy_denied`) of an object with `external_secrets` missing from `targets` or not bound,
  even without `secret_allowlist` (errata 7). Without a compose policy the behaviour is upstream.
  The logging rule also applies to the last good of a rollback: a last good recorded under
  `poli.1` that declares `logging` with no `default.logging` (or a `com.docker.compose.*` label)
  is denied by the policy in force, so its rollback ends `rollback_failed`, not only the new
  commit `policy_denied`.
- **Logging stays out of the app's hands.** Portico never puts `labels-regex`, `env` or
  `env-regex` in `default.logging` or in the daemon `log-opts`: the label check sees only the
  service labels, and labels that come from the image or from `build.labels` bypass it, so an
  option that copies labels or variables into the journal would let an app forge
  `COM_DOCKER_COMPOSE_*` fields.
- **Token sink mounted as a directory.** With `SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD`,
  `SECRET_PROVIDER_ACCESS_TOKEN_FILE` points to `<mounted dir>/<file>` (for example
  `/run/portico-runtime` on `/run/portico-runtime:ro`): a bind of the single file does not see the
  rename with which the `bao agent` rewrites it.
- **Store versions.** `GetFailure` reads `version: 1` (no `content_key`, keeps its state and
  releases on a new commit) and `version: 2`; every write is version 2. A downgrade to `poli.1`
  reads a version 2 record as corrupt, which (by «Never a blind deploy») blocks the target on its
  current revision until a new commit or the unblock action. The last good stays `version: 1`.

## Formats

- **RBAC** (`RBAC_POLICY_FILE`): Argo CD CSV, `p, <subject|role>, <resource>, <action>,
  <object>, <allow|deny>` (effect mandatory) and `g, <group|sub>, role:<name>`; deny by default,
  an explicit deny wins. Resources and actions per route and per MCP tool: spec, «Mappa».
- **Compose policy** (`COMPOSE_POLICY_FILE`): YAML `version: 1`, `default` and `targets`, each
  with `repository`, `reference`, `config_target`, `bind_allowlist`, `cap_add_allowlist`.
  `reference` matches like `git.ReferenceMatches`, so a tag is declared in full form
  `refs/tags/<tag>`. Rules: spec, «Compose policy». Since `poli.2`:
  - `secret_allowlist` (in `default` and per target; a target's list replaces the default's):
    absent means no check; a present empty list allows no reference; `null`/`~` is a parse
    error. Entries: `kv:[<ns>:]<engine>:<prefix>` with segments in `[A-Za-z0-9._-]`, full
    `pki:`/`pki-role:` references, or literal prefixes for providers other than OpenBao;
  - `logging` (only in `default`, a non-null `logging` on a target is a parse error): `driver` (required,
    not padded) and `options` (string map), the only logging a service may declare.
- **Deploy config `rollback.unblock_on`**: `commit` (default, also when absent) or `content`; any
  other value fails the deploy config parse. `omitempty`, so `deploy.Config.Hash()` is unchanged
  without it.
- **`SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD`** (bool, OpenBao only): needs
  `SECRET_PROVIDER_ACCESS_TOKEN_FILE` and is refused with `SECRET_PROVIDER_ACCESS_TOKEN`.
- **Failure store** (`<data>/fork/failures/<ctx>/<project>.json`): `version: 2`, with the optional
  `content_key` (`tree:<hash>;config:<hash>`) written only with `unblock_on: content`;
  `version: 1` is still read.
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
- Secret allowlist limits: for providers other than OpenBao an entry is a literal prefix
  (`strings.HasPrefix`), not proven non-permissive for path-based providers; for scheduled jobs,
  cert rotation and self-apply `forkSecretGate` uses the identity declared by the target in
  `targets` and relies on `forkJobGate` for the repository check —
  https://app.notion.com/p/3fca27896a538176a3cacf18ff0a1fd9
- Content key limits (`rollback.unblock_on: content`):
  - OCI sources and deploy configs without `Internal.File` (inline, auto-discovery) silently fall
    back to commit mode, with one Warn per failure;
  - the `config` half is the blob of the whole deploy config file: in a multi-document file a
    change of any document releases this target too;
  - files read **outside `working_dir`** are not in the key: `include` or `env_file` with paths
    that go up (`../`), `remote:` files, `external_secrets_files` outside the directory; a change
    only there does not release the target;
  - the `config` half is read from disk, so it belongs to the revision of the run, while the
    `tree` half comes from the failed commit (identical today, since the record is written by the
    run of that commit);
  - with `repository_url` the `tree` half comes from the deploy repository and the `config` half
    from the file on disk (rollback with `repository_url` was already a known limit);
  - if a commit **removes `working_dir`**, `TreeHashAt` fails and the target stays poisoned (fail
    closed) until a commit with the directory or the `unblock` action;
  - pre-deploy order: secrets are resolved (`stage_2_pre-deploy.go:266-292`) before
    `rollbackPoisoned` (`:356`), so with OpenBao down a poisoned target already fails at
    resolution and does not reach the poison check; the key itself does not use the vault —
    https://app.notion.com/p/3fca27896a538162849eecc4af839f71
- `failure_kind` in the outcome (spec 3c, «useful») is not done —
  https://app.notion.com/p/3fca27896a5381489b6cde008677b74b
- The fork CI has no permanent `-race` job: the token reload race test ran with `-race` once, in a
  temporary CI step of PR #9 — https://app.notion.com/p/3fca27896a538183ae97daa6ec9dc280
- Upstream `GetCertSerial` panics when the `pki/certs/detailed` response has no `key_info` —
  https://app.notion.com/p/3fca27896a5381a4afa2e9d9d66bdc3e
- `runForkPreDeployStage` (`internal/stages/fork_policy.go`) is at 80% statement coverage since
  `poli.1`, below the 100% rule for fork files —
  https://app.notion.com/p/3fca27896a5381f6a339e858dca1661e
- Object names are not case-folded; `apply-self` in a separate process has no job gate; the
  compose policy `label=` rule blocks only `spc_t` and `LoadCompose` `WithOsEnv` exposes the
  Doco-CD environment to `${...}`; two outboxes may run on the same directory during the
  self-update overlap; the number of upstream files touched should shrink —
  https://app.notion.com/p/3fca27896a5381a28ab7e22caa26d545

## Spec errata (`poli.2`)

The `poli.2` plan corrects the Portico v2 specs in eight points; a separate errata PR on the specs
in `lucapolignone/portico` carries them.

1. **Spec 4, `docker.ForkSecretGate`.** The spec gives `func(contextName, project string, refs
   map[string]string) error`, called in `prepareComposeScheduledDeployConfig`, which does not
   receive the `command.Cli` and so does not know the Docker context. The fork uses
   `func(object, provider string, refs map[string]string) error`; the object comes from
   `loadComposeScheduledProjectAll`, computed as `checkForkJobGate` does, through the
   `context.Context`. A reloaded deploy config naming another object is refused.
2. **Spec 4, `CheckSecrets`.** Choosing between the OpenBao split and the literal comparison of the
   other providers needs the provider name, which the `File` does not know:
   `(*File).CheckSecrets(id Identity, provider string, refs map[string]string) []Violation`, and
   the caller passes `SecretProvider.Name()`.
3. **Spec 5, compose labels.** The spec denies labels prefixed `com.docker.compose.`. The `journald`
   driver normalizes label names with `sanitizeKeyMod` (moby
   `daemon/logger/journald/journald.go:76-91`), so `COM.DOCKER.COMPOSE.PROJECT` and
   `com_docker_compose_project` give the same field. The fork denies every label whose normalized
   form starts with `COM_DOCKER_COMPOSE_`.
4. **Spec 5, rule always on.** With a compose policy the rule holds for every target, also with a
   policy written for `poli.1`: without `default.logging`, a `services.*.logging` that `poli.1`
   accepted becomes `policy_denied`. Intended, and stated in the release notes.
5. **Spec 3c, store version 2.** The spec covers only the downgrade. On upgrade `GetFailure`
   accepts `version: 1` (without `content_key`) and `version: 2`; every write uses 2.
6. **Spec 3c, content key.** The `config` half is the git blob hash of the deploy config file read
   from disk (`deploy.Config.Internal.File`), `plumbing.ComputeHash(plumbing.BlobObject, data)`:
   the hash the file has in the repository at that revision, without looking the file up in the
   mirror, which with `repository_url` would be another one. The `tree` half comes from the mirror
   of the deploy repository (`RepositoryData.MirrorDir`).
7. **Spec 4, object missing or not bound.** Per the spec, without `secret_allowlist` nothing is
   checked; but with a compose policy an object missing from `targets`, or with another identity,
   would read the secrets before `checkForkPolicies` denies it. The fork gives it the `target`
   violation as soon as there are references, with or without allowlist (fail closed). Likewise
   `ForkSecretGate`, with the gate set and no object in the context, refuses.
8. **Spec 4, token sink.** The `bao agent` sink is mounted in the Doco-CD container as a
   **directory** (e.g. `/run/portico-runtime` on `/run/portico-runtime:ro`) and
   `SECRET_PROVIDER_ACCESS_TOKEN_FILE` points to the file inside. A bind of the single file does
   not see the rename with which the agent rewrites it.
