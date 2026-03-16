# E2E Framework Rewrite Design

## Goal

Rebuild `test/e2e` around `sigs.k8s.io/e2e-framework` so the repository has one authoritative end-to-end test stack:

- shell scripts handle environment bootstrap only
- all executable E2E behavior lives in Go suites
- legacy shell cases remain only as source references

This is an intentional hard cut. The repository will stop maintaining two executable E2E systems.

## Why Change Direction

The current repository is in an intermediate state:

- environment setup has already been split into `test/e2e/hack`
- several shell scenarios have been migrated to Go live tests
- suite wrappers and legacy compatibility glue still exist

That intermediate state was useful to prove the direction, but it is not a good long-term architecture. The repository still carries:

- old shell suite directories with business logic
- `test/e2e/tests` and `test/e2e/framework` as a custom test harness
- compatibility runners that preserve old calling conventions

For a Kubernetes project with real KIND-backed suites, this should be simplified into one standard approach instead of growing a private mini-framework indefinitely.

## Current Problems

The current E2E stack has five structural issues.

### 1. Two authoritative styles still exist

The repository currently has:

- legacy shell suites under `test/e2e/01-*` through `test/e2e/07-*`
- Go live tests under `test/e2e/tests`

Even when wrappers point to Go, the directory structure still suggests both models are valid.

### 2. Test infrastructure is split awkwardly

There is no clear distinction between:

- cluster bootstrap
- suite bootstrap
- fast-sandbox domain fixtures
- helper unit tests

`test/e2e/framework` currently mixes E2E support code with helper-only tests that are not themselves E2E.

### 3. Legacy shell source still shapes the runtime structure

The existing suite and directory names are inherited from shell history rather than the best shape for the new system. This is fine for migration reference, but it should not define the new runtime layout.

### 4. The repository still maintains compatibility glue

Files such as:

- `test/e2e/common.sh`
- `test/e2e/hack/legacy_suite.sh`
- `test/e2e/*/test.sh`

exist only because the migration stopped halfway. They add maintenance surface without improving test quality.

### 5. There is no standard Kubernetes E2E harness

The repository already uses plain `go test`, but it still owns too much generic suite lifecycle code itself. `e2e-framework` provides a better standard base for:

- environment lifecycle
- feature grouping
- test labels and filtering
- Kubernetes resource access

## Design Principles

The rewrite follows these rules.

1. `test/e2e` contains executable E2E tests only.
2. Shell remains only where shell is the right tool: KIND lifecycle, image loading, installation, destructive cleanup, and fault-injection setup.
3. Every executable business scenario runs through Go.
4. The new stack uses `e2e-framework` for generic Kubernetes E2E structure, not for fast-sandbox-specific domain assertions.
5. Legacy shell cases remain in the repository as historical specifications, not as runnable test entrypoints.
6. Helper-only unit tests do not live under `test/e2e`.

## Target Architecture

### 1. Environment Layer

Location:

- `test/e2e/hack/`

Responsibilities:

- create, delete, and reset KIND clusters
- build and load images
- install CRDs, RBAC, controller, agent, janitor
- prepare external fixtures such as webhooks or chaos helpers
- provide top-level entrypoints such as `make e2e-smoke`

Non-responsibilities:

- no case logic
- no business assertions
- no dynamic suite discovery

### 2. Suite Layer

Location:

- `test/e2e/suites/`

Responsibilities:

- define all executable E2E scenarios
- group cases by system capability, not by legacy shell history
- use `e2e-framework` environment and feature APIs
- expose label-based execution boundaries such as `smoke`, `cli`, `slow`, `chaos`

Expected suite packages:

- `test/e2e/suites/basicvalidation`
- `test/e2e/suites/scheduling`
- `test/e2e/suites/lifecycle`
- `test/e2e/suites/janitor`
- `test/e2e/suites/advanced`
- `test/e2e/suites/cli`
- `test/e2e/suites/recovery`

### 3. Support Layer

Location:

- `test/e2e/support/`

Responsibilities:

- build fast-sandbox-specific fixtures and helpers on top of `e2e-framework`
- manage namespace allocation and test-scoped cleanup
- create and wait on `SandboxPool` and `Sandbox`
- wrap `fsb-ctl`
- manage port-forward processes
- dump diagnostics on failure

This layer is where repository-specific logic belongs. `e2e-framework` supplies the generic base; `test/e2e/support` supplies the domain behavior.

### 4. Legacy Reference Layer

Location:

- `test/e2e/legacy/`

Responsibilities:

- preserve original shell cases as reference artifacts
- document legacy scenario names and expected behaviors

Non-responsibilities:

- no execution
- no CI integration
- no wrappers

## Directory Layout

```text
test/e2e/
  hack/
    _env.sh
    cluster.sh
    images.sh
    install.sh
    run-smoke.sh
    run-cli.sh
    run-chaos.sh
    chaos/
  suites/
    basicvalidation/
    scheduling/
    lifecycle/
    janitor/
    advanced/
    cli/
    recovery/
  support/
    suiteenv/
    fixtures/
    cli/
    portforward/
    diagnostics/
  legacy/
    01-basic-validation/
    02-scheduling-resources/
    03-lifecycle/
    04-cleanup-janitor/
    05-advanced-features/
    06-cli-integration/
    07-fault-recovery/
  README.md
```

The current `test/e2e/framework` and `test/e2e/tests` directories are transitional and will be removed.

## `e2e-framework` Usage Model

The repository will use `e2e-framework` as the standard Kubernetes E2E harness in these places.

### Environment bootstrap

Each suite package will run through a shared suite environment bootstrap that:

- discovers kubeconfig or cluster context
- creates and cleans per-test namespaces
- exposes an `env.Environment`
- registers diagnostics hooks

KIND creation itself stays in shell because that remains the right place for repository-specific cluster bootstrapping and image loading.

### Feature organization

Each executable scenario becomes a feature-oriented test. The intended pattern is:

- one case equals one system behavior
- one feature contains one or more explicit assessments
- labels declare suite semantics such as `smoke`, `cli`, `chaos`

This gives a uniform test shape across the repository.

### Kubernetes clients and resources

Generic client/resource access should come from `e2e-framework` where practical. Domain-specific higher-level helpers remain in `test/e2e/support`.

Examples:

- use framework-backed resource clients to create namespaces and generic resources
- use fast-sandbox fixtures to create `SandboxPool` and `Sandbox` with repository defaults
- use support wait helpers for domain conditions like assignment, expiry, or recovery

## What Will Be Rewritten

### Existing Go live tests

All existing live tests under `test/e2e/tests` will be rewritten into the new `test/e2e/suites` structure. This is a style rewrite as well as a directory move.

The current Go tests proved behavior and timing, but they are still organized around the transitional harness.

### Existing support code

Current support code under `test/e2e/framework` and `test/e2e/tests/support` will be reorganized into the new `test/e2e/support` packages.

Only code that helps executable E2E tests remains under `test/e2e`. Helper-only tests move out or are deleted if their value is purely transitional.

### Existing shell suites

The shell suite directories are preserved as source material, then moved under `test/e2e/legacy`. Their `test.sh` files and execution glue will be deleted after the new suites cover those behaviors.

## Migration Strategy

This rewrite proceeds in four phases.

### Phase 1: Establish the new base

- add `sigs.k8s.io/e2e-framework`
- create `test/e2e/suites`
- create `test/e2e/support`
- build shared suite bootstrap and feature conventions

### Phase 2: Rewrite already-migrated cases

Rewrite all current Go live cases into the new suite shape:

- CRD validation
- namespace isolation
- autoscaling
- resource slot
- port conflict
- same-name recreate
- auto expiry
- CLI logs

At the end of this phase, `run-smoke.sh` and `run-cli.sh` point only at `test/e2e/suites`.

### Phase 3: Port remaining legacy behaviors

Use the shell cases under `legacy/` as the source specification to rewrite the remaining behaviors into `e2e-framework` suites.

Priority order:

1. `port-validation`
2. `env-workingdir`
3. `graceful-shutdown`
4. `cli-cache`
5. `update-reset`
6. janitor scenarios
7. advanced and recovery chaos scenarios

### Phase 4: Remove transitional glue

- remove `test/e2e/framework`
- remove `test/e2e/tests`
- remove `test/e2e/common.sh`
- remove `test/e2e/hack/legacy_suite.sh`
- remove all suite `test.sh`
- move shell source directories under `test/e2e/legacy`

## Test Semantics

To keep `test/e2e` honest, the repository adopts these semantic rules.

### What belongs in `test/e2e`

- tests that require a real Kubernetes cluster
- tests that create real CRDs, Pods, or workloads
- tests that exercise fast-sandbox through real control-plane and node-side behavior
- tests that exercise the real CLI against a live cluster

### What does not belong in `test/e2e`

- helper-only unit tests
- pure command-construction tests
- isolated polling utility tests
- filesystem-only tests with no cluster dependency

Those belong next to the code they validate or under a non-E2E integration/unit test location.

## Risks and Controls

### Risk: the rewrite temporarily destabilizes smoke coverage

Control:

- keep shell bootstrap stable while suite code is replaced
- rewrite already-passing Go live cases first
- run live smoke and CLI suites after each chunk

### Risk: `e2e-framework` integration adds churn without enough benefit

Control:

- use it only for generic suite lifecycle and feature composition
- keep domain-specific waiting and fixtures in repository code

### Risk: helper test deletions reduce confidence

Control:

- replace helper-only tests with live suite verification where appropriate
- keep unit tests near support code only if they validate repository-specific logic rather than transitional scaffolding

## Non-Goals

This rewrite does not:

- change product behavior
- replace KIND
- introduce Ginkgo
- preserve old shell runner compatibility
- keep old suite directory structure as the runtime model

## Expected Outcome

After the rewrite:

- `make e2e-*` drives one E2E system only
- shell scripts are environment-only
- all executable business scenarios are `e2e-framework` suites in Go
- legacy shell cases remain as human-readable source references
- `test/e2e` reflects true E2E semantics instead of a mix of migration scaffolding and runtime code
