# doco-cd - Docker Compose Continuous Deployment

> [!NOTE]
> **This is a fork.** `luca-polignone-org/doco-cd` is a fork of [kimdre/doco-cd](https://github.com/kimdre/doco-cd), based on `v0.127.0`, maintained for Portico v2.
> It adds JWT (JWKS) authentication with Argo CD-style RBAC for REST and MCP, a compose policy bound to the target repository, an on-disk failure store with local rollback to the last good deployment, a `pre_rollback` veto hook and signed outcome events delivered through an outbox. See [NOTICE](NOTICE) for the list of modifications.
>
> Every fork feature is off unless its configuration keys are set (`JWT_JWKS_URL`, `RBAC_POLICY_FILE`, `COMPOSE_POLICY_FILE`, `HOOK_PRE_ROLLBACK_URL`, `HOOK_OUTCOME_URL`, and `rollback` in the deploy configuration); without them it behaves as upstream.
>
> Fork releases are tagged `vX.Y.Z-poli.N` and their images are published to `ghcr.io/luca-polignone-org/doco-cd` (linux/amd64, linux/arm64, linux/arm/v7), pinned by the digest written in each release's notes. The documentation below is upstream's.

## GitOps for Docker Compose

<img src="https://raw.githubusercontent.com/kimdre/doco-cd/main/wiki/docs/images/doco-cd_logo.svg" alt="Doco-CD Logo" height="48px" />

[![GitHub Release](https://img.shields.io/github/v/release/kimdre/doco-cd?display_name=tag&label=Release&color=47c72a&labelColor=404951)](https://github.com/kimdre/doco-cd/releases)
[![CodeQL](https://github.com/kimdre/doco-cd/actions/workflows/github-code-scanning/codeql/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/github-code-scanning/codeql)
[![Tests](https://github.com/kimdre/doco-cd/actions/workflows/test.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/test.yaml)
[![Build Image](https://github.com/kimdre/doco-cd/actions/workflows/build.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/build.yaml)
[![Image Vulnerability Scan](https://github.com/kimdre/doco-cd/actions/workflows/image-vulnerability-scanning.yml/badge.svg?event=schedule)](https://github.com/kimdre/doco-cd/actions/workflows/image-vulnerability-scanning.yml)


Doco-CD is a lightweight, declarative GitOps continuous delivery tool that automatically deploys and updates Docker Compose projects/services and Swarm stacks using polling and webhooks.

You can think of it as a simple Portainer or ArgoCD alternative for Docker.

## Documentation

You can find the documentation at [doco.cd](https://doco.cd/latest/).

## Features

- Easy to set up and use with minimal configuration.
- Runs with a minimal (distroless) image with tiny RAM and CPU requirements.
- Supports various [Git providers](https://doco.cd/latest/#supported-git-providers).
- Deploy via [webhooks](https://doco.cd/latest/Getting-Started/#webhooks) and/or [polling](https://doco.cd/latest/Getting-Started/#polling).
- Supports both Docker Compose projects and Swarm stacks in [Swarm mode](https://doco.cd/latest/Advanced/Swarm-Mode/).
- Supports [OCI artifacts](https://doco.cd/latest/Advanced/OCI/Artifact-Usage/) and Git repositories for deployments.
- Supports various [external secret providers](https://doco.cd/latest/External-Secrets/) and encryption with [SOPS](https://doco.cd/latest/Advanced/Encryption/).
- Deploy to multiple Docker hosts with [Docker Contexts](https://doco.cd/latest/Advanced/Docker-Contexts/).
- Control when deployments may happen with timezone-aware [sync windows](https://doco.cd/latest/Advanced/Sync-Windows/) and cron-scheduled [polling](https://doco.cd/latest/Poll-Settings/#cron-schedules).
- Provides [notifications](https://doco.cd/latest/Advanced/Notifications/) and [Prometheus metrics](https://doco.cd/latest/Endpoints/Metrics/) for monitoring.
- [Job Scheduling](https://doco.cd/latest/Advanced/Job-Scheduling/), [Pre/Post-deployment scripts](https://doco.cd/latest/Advanced/Pre-Post-Deployment-Scripts/), [REST API](https://doco.cd/latest/Endpoints/REST-API/) and [MCP Server](https://doco.cd/latest/Endpoints/MCP-Server/) for advanced automation and integrations.

## Community

- Ask questions or discuss ideas on [GitHub Discussions](https://github.com/kimdre/doco-cd/discussions)
- Report bugs or suggest features by [opening an issue](https://github.com/kimdre/doco-cd/issues/new/choose)

## Contributing

Contributions are welcome! Please see the [contributing guidelines](https://doco.cd/latest/Contributing/) for more information.

## Support the Project

If you would like to support the development of Doco-CD, please consider becoming a sponsor on [GitHub Sponsors](https://github.com/sponsors/kimdre).

Doco-CD is proudly supported by

<a href="https://jb.gg/OpenSource">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://www.jetbrains.com/company/brand/img/logo_jb_dos_3.svg" />
   <source media="(prefers-color-scheme: light)" srcset="https://resources.jetbrains.com/storage/products/company/brand/logos/jetbrains.svg" />
   <img alt="JetBrains logo" src="https://resources.jetbrains.com/storage/products/company/brand/logos/jetbrains.svg" />
 </picture>
</a>

[![sponsors badge](https://readme-contribs.as93.net/sponsors/kimdre?shape=circle&fontSize=14)](https://github.com/sponsors/kimdre)

## Star History

<a href="https://www.star-history.com/?repos=kimdre%2Fdoco-cd&type=date&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/chart?repos=kimdre/doco-cd&type=date&theme=dark&legend=top-left&sealed_token=saZ0hV4u45f86P2hpUFWpOZm-TTGM2b4M-z2Ji3FRAzBKzsZ5l1JmPPstp4FYVSkLI4mqT3OqCnGxuiJEhZ1KDWZs7P39Slsvd_dIJfC1tCRRsi5HafLag" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/chart?repos=kimdre/doco-cd&type=date&legend=top-left&sealed_token=saZ0hV4u45f86P2hpUFWpOZm-TTGM2b4M-z2Ji3FRAzBKzsZ5l1JmPPstp4FYVSkLI4mqT3OqCnGxuiJEhZ1KDWZs7P39Slsvd_dIJfC1tCRRsi5HafLag" />
   <img alt="Star History Chart" src="https://api.star-history.com/chart?repos=kimdre/doco-cd&type=date&legend=top-left&sealed_token=saZ0hV4u45f86P2hpUFWpOZm-TTGM2b4M-z2Ji3FRAzBKzsZ5l1JmPPstp4FYVSkLI4mqT3OqCnGxuiJEhZ1KDWZs7P39Slsvd_dIJfC1tCRRsi5HafLag" />
 </picture>
</a>
