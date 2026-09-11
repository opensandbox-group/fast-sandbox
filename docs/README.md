# Fast Sandbox documentation

Fast Sandbox documentation is organized by reader intent:

- **Getting started** provides a runnable first experience.
- **Concepts** explain architecture, invariants, and failure semantics.
- **Guides** describe operational and integration tasks.
- **Reference** defines stable API, CLI, configuration, and capability contracts.

## Getting started

- [Quick Start](getting-started/quickstart.md)

## Concepts

- [Architecture](concepts/architecture.md)
- [Control plane](concepts/control-plane.md)
- [Sandbox lifecycle](concepts/sandbox-lifecycle.md)
- [Runtime model](concepts/runtimes.md)
- [BoxLite runtime](concepts/boxlite-runtime.md)
- [Private networking](concepts/networking.md)
- [Data plane](concepts/data-plane.md)
- [Infra Components](concepts/infra-components.md)
- [Sandbox Actions](concepts/sandbox-actions.md)
- [Scheduling and capacity](concepts/scheduling-and-capacity.md)

## Guides

- [Deployment](guides/deployment.md)
- [OpenSandbox integration](guides/opensandbox-integration.md)
- [OpenSandbox Execd](guides/opensandbox-execd.md)
- [SandboxTemplate golden images](guides/sandboxtemplate-golden-images.md)
- [Sandbox Snapshots](guides/sandbox-snapshots.md)
- [Private registries](guides/private-registries.md)
- [Secure runtimes](guides/secure-runtimes.md)
- [Runtime node installation](guides/runtime-node-installation.md)
- [Runtime environments](guides/runtime-environments.md)
- [Sandbox Actions](guides/sandbox-actions.md)
- [Observability](guides/observability.md)
- [Performance](guides/performance.md)
- [Testing](guides/testing.md)
- [Firecracker runtime E2E](guides/firecracker-runtime-e2e.md)
- [Firecracker full-chain E2E](guides/firecracker-chain-e2e.md)

## Reference

- [API](reference/api.md)
- [Infra Components](reference/infra-components.md)
- [fastctl](reference/fastctl.md)
- [Configuration](reference/configuration.md)
- [Runtime support](reference/runtime-support.md)

## Documentation policy

Published documentation describes the current product contract:

- concepts explain why the system works this way;
- guides explain how to deploy, operate, or integrate it;
- reference pages define exact fields, APIs, commands, and defaults.

Working design notes, implementation plans, review notes, and branch-specific
investigations belong under the repository-local `.dev/` directory. The
directory is ignored by Git and is never a source of truth. When a design is
implemented, its stable behavior must be incorporated into the relevant
concept, guide, and reference pages in the same change.

Designs that require review across contributors belong in a GitHub Issue,
Pull Request, or the owning project's formal proposal process. They must not
depend on ignored local files.

All documentation except the root `README_ZH.md` is maintained in U.S. English.
Branch-specific plans, implementation logs, and superseded designs are not
part of the published documentation set.
