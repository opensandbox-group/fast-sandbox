# Fast Sandbox Agent Guide

Read the relevant source and documentation before editing. Follow a nearer `AGENTS.md` when present.

## Code map

Fast Sandbox is a Kubernetes-native sandbox runtime. Start with [Architecture](docs/concepts/architecture.md) and the [documentation index](docs/README.md).

- `api/`: public CRD types and Fast-Path protobuf contracts.
- `internal/controlplane/`: scheduling, orchestration, and reconciliation.
- `internal/fastlet/`: local admission and sandbox lifecycle.
- `internal/runtime/`: runtime adapters; `firecracker/agent/` handles node-level artifact delivery and host readiness.
- `internal/dataplane/`: routing, authentication, and proxies.
- `cmd/`: binary entry points; `pkg/sandboxclient/` and `sdk/python/`: clients.
- `config/`, `build/`, `scripts/`: deployment and tooling; `test/`: E2E and performance tests.

## Working rules

- Make the smallest complete change; preserve local style and unrelated user changes.
- Define success criteria, investigate with logs and tests, and avoid guessing behavior.
- Kubernetes owns durable intent; Fastlet owns atomic admission. Preserve idempotency, generation fencing, and persistence before runtime side effects. Keep shared contracts independent of implementations.
- Keep lifecycle/routing separate from user protocols such as Execd. Readiness alone does not prove protocol functionality.
- Keep public contracts, implementation, clients, configuration, and docs aligned. Discuss breaking changes and migration before implementation.
- Change generator sources and run `make generate`; include derived protobuf, deepcopy, and CRD output. Never fix generated files alone.
- Meaningful comments only: explain why, not what — add or keep comments only for non-obvious invariants, constraints, edge cases, workarounds, or required public API docs; omit comments that merely narrate self-explanatory code or change history, avoid boilerplate and commented-out code, and update or remove touched comments when behavior changes.

## Validation

Use the toolchain in `go.mod`. Start with affected packages, then run relevant checks:

```bash
make test SCOPE=unit
make test SCOPE=python   # Python SDK changes; install sdk/python[dev] first
make test SCOPE=race     # Concurrency changes
make lint
make verify             # Generation, lint, generated-output check, and Go tests
```

See [Testing](docs/guides/testing.md) for E2E and host setup. Runtime E2E requires Linux; direct Firecracker tests also need root, KVM, TUN, and XFS reflink. The pinned protoc bootstrap targets Linux/x86_64. Check prerequisites before running host scripts, and report skipped or unavailable checks.

## Docs and proposals

Write docs and proposals in U.S. English. Keep paragraphs on one source line. Put current behavior in `docs/`, temporary notes in Git-ignored `.dev/`, and reviewed designs in `fseps/`.

Follow the [FSEP guide](fseps/CONTRIBUTING.md): discuss major changes first, write proposals when needed, and cover compatibility, testing, and rollback. Small fixes go directly to a PR.

## Pull requests and issues

- Write PR and Issue titles and descriptions in English; keep them concise and specific.
- Keep each PR focused on one problem. Explain the reason for the change, report validation results, and link existing Issues or FSEPs.
- For bugs, include the environment, reproduction steps, and expected versus actual behavior. For feature requests, describe the problem, use case, and desired behavior.

Before handoff, review the diff and summarize the change, validation results, and any remaining limitations.
