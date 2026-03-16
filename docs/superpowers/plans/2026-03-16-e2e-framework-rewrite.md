# E2E Framework Rewrite Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the transitional shell-plus-custom-Go E2E stack with an `e2e-framework`-based suite system where shell only manages environment bootstrap and all executable business scenarios run as Go E2E suites.

**Architecture:** The work creates a new `test/e2e/suites` plus `test/e2e/support` structure, rewrites the current Go live tests into `e2e-framework` feature suites, ports the remaining shell scenarios from legacy source into Go, then removes transitional runners and directories. Legacy shell files are preserved as references under `test/e2e/legacy` but never executed.

**Tech Stack:** Go, Bash, KIND, kubectl, sigs.k8s.io/e2e-framework, existing fast-sandbox CRDs/controllers/agent/janitor, Makefile

---

## File Structure

### Create

- `docs/superpowers/specs/2026-03-16-e2e-framework-rewrite-design.md`
- `docs/superpowers/plans/2026-03-16-e2e-framework-rewrite.md`
- `test/e2e/suites/basicvalidation/`
- `test/e2e/suites/scheduling/`
- `test/e2e/suites/lifecycle/`
- `test/e2e/suites/janitor/`
- `test/e2e/suites/advanced/`
- `test/e2e/suites/cli/`
- `test/e2e/suites/recovery/`
- `test/e2e/support/suiteenv/`
- `test/e2e/support/fixtures/`
- `test/e2e/support/cli/`
- `test/e2e/support/portforward/`
- `test/e2e/support/diagnostics/`
- `test/e2e/legacy/`

### Modify

- `go.mod`
- `go.sum`
- `Makefile`
- `test/e2e/README.md`
- `test/e2e/hack/run-smoke.sh`
- `test/e2e/hack/run-cli.sh`
- `test/e2e/hack/cluster.sh`
- `test/e2e/hack/images.sh`
- `test/e2e/hack/install.sh`

### Remove later

- `test/e2e/framework/`
- `test/e2e/tests/`
- `test/e2e/common.sh`
- `test/e2e/hack/legacy_suite.sh`
- `test/e2e/*/test.sh`

## Chunk 1: Establish the New `e2e-framework` Base

### Task 1: Add the dependency and define suite boundaries

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `test/e2e/README.md`

- [ ] **Step 1: Add a failing compile target for the new suite bootstrap**

Create a placeholder package import in a new support package so `go test ./test/e2e/suites/...` fails due to missing bootstrap code.

- [ ] **Step 2: Add `sigs.k8s.io/e2e-framework` to module dependencies**

Run: `go get sigs.k8s.io/e2e-framework@latest`
Expected: `go.mod` and `go.sum` include the dependency.

- [ ] **Step 3: Rewrite `test/e2e/README.md` around the new architecture**

Document:
- `hack/` is environment-only
- `suites/` is the only executable E2E location
- `support/` contains fast-sandbox-specific E2E support code
- `legacy/` is source reference only

- [ ] **Step 4: Verify module and doc state**

Run:
```bash
go test ./test/e2e/suites/... -run TestDoesNotExist -count=1
```

Expected: the command fails because the bootstrap packages do not exist yet, proving the new entrypoint is now the target.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum test/e2e/README.md
git commit -m "test: introduce e2e-framework suite baseline"
```

## Chunk 2: Build Shared E2E Support

### Task 2: Create suite environment bootstrap

**Files:**
- Create: `test/e2e/support/suiteenv/env.go`
- Create: `test/e2e/support/suiteenv/env_test.go`

- [ ] **Step 1: Write the failing tests**

Cover:
- kubeconfig/config discovery
- namespace name generation
- cleanup callback registration
- suite label parsing helpers if added

- [ ] **Step 2: Run the support package tests**

Run: `go test ./test/e2e/support/suiteenv -count=1 -v`
Expected: FAIL

- [ ] **Step 3: Implement the minimal suite environment**

Add:
- shared environment constructor
- test namespace allocator
- test cleanup registration
- controller namespace discovery

- [ ] **Step 4: Run the support package tests**

Run: `go test ./test/e2e/support/suiteenv -count=1 -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add test/e2e/support/suiteenv
git commit -m "test: add e2e suite environment support"
```

### Task 3: Move fast-sandbox fixtures into support

**Files:**
- Create: `test/e2e/support/fixtures/pool.go`
- Create: `test/e2e/support/fixtures/sandbox.go`
- Create: `test/e2e/support/fixtures/fixtures_test.go`
- Modify: `test/e2e/support/suiteenv/env.go`

- [ ] **Step 1: Write the failing tests**

Cover:
- create namespace-scoped `SandboxPool`
- create namespace-scoped `Sandbox`
- wait for bound/running state
- wait for expiry
- assert unassigned state when scheduling should not happen

- [ ] **Step 2: Run the fixture tests**

Run: `go test ./test/e2e/support/fixtures -count=1 -v`
Expected: FAIL

- [ ] **Step 3: Implement the minimal fixture layer**

Use typed fast-sandbox CRDs and framework/resource clients. Do not shell out to `kubectl` for normal CRUD.

- [ ] **Step 4: Run the fixture tests**

Run: `go test ./test/e2e/support/fixtures -count=1 -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add test/e2e/support/fixtures test/e2e/support/suiteenv
git commit -m "test: add fast-sandbox e2e fixtures"
```

### Task 4: Move CLI, port-forward, and diagnostics support

**Files:**
- Create: `test/e2e/support/cli/client.go`
- Create: `test/e2e/support/cli/client_test.go`
- Create: `test/e2e/support/portforward/portforward.go`
- Create: `test/e2e/support/portforward/portforward_test.go`
- Create: `test/e2e/support/diagnostics/dump.go`
- Create: `test/e2e/support/diagnostics/dump_test.go`

- [ ] **Step 1: Write failing tests**

Cover:
- CLI command construction
- managed port-forward process lifecycle
- diagnostics target resolution and dump command selection

- [ ] **Step 2: Run the support package tests**

Run:
```bash
go test ./test/e2e/support/cli -count=1 -v
go test ./test/e2e/support/portforward -count=1 -v
go test ./test/e2e/support/diagnostics -count=1 -v
```

Expected: FAIL

- [ ] **Step 3: Implement the minimal support packages**

Keep the process model test-scoped only. No global `pkill`, no suite-global mutable state.

- [ ] **Step 4: Run the support package tests**

Run:
```bash
go test ./test/e2e/support/cli -count=1 -v
go test ./test/e2e/support/portforward -count=1 -v
go test ./test/e2e/support/diagnostics -count=1 -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add test/e2e/support/cli test/e2e/support/portforward test/e2e/support/diagnostics
git commit -m "test: add shared e2e support utilities"
```

## Chunk 3: Rewrite Existing Go Live Cases into `suites/`

### Task 5: Rewrite smoke-oriented suites

**Files:**
- Create: `test/e2e/suites/basicvalidation/*.go`
- Create: `test/e2e/suites/scheduling/*.go`
- Create: `test/e2e/suites/lifecycle/*.go`
- Create: `test/e2e/suites/recovery/*.go`
- Create: package bootstrap files as needed
- Modify: `test/e2e/hack/run-smoke.sh`

- [ ] **Step 1: Write a failing smoke suite bootstrap**

Create one package-level suite test file that imports `e2e-framework` and references the new support packages.

- [ ] **Step 2: Run the smoke suites to verify failure**

Run:
```bash
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/basicvalidation ./test/e2e/suites/scheduling ./test/e2e/suites/lifecycle ./test/e2e/suites/recovery -count=1 -v
```

Expected: FAIL because the cases are not implemented yet.

- [ ] **Step 3: Rewrite the existing migrated behaviors into new suite style**

Rewrite:
- `TestSandboxCRDValidation`
- `TestNamespaceIsolation`
- `TestAutoscalingExpandsPoolForSecondSandbox`
- `TestResourceSlotEnforcesPerPodCapacity`
- `TestPortConflictSchedulesToDifferentAgents`
- `TestRecreateSameNameSandbox`
- `TestSandboxAutoExpiry`

Use `e2e-framework` features, labels, and shared suite bootstrap instead of the transitional support shape.

- [ ] **Step 4: Point `run-smoke.sh` at `test/e2e/suites/...` only**

Remove references to `test/e2e/tests/...`.

- [ ] **Step 5: Run live smoke verification**

Run:
```bash
make e2e-smoke
```

Expected: PASS in the real KIND-backed environment.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/suites test/e2e/hack/run-smoke.sh
git commit -m "test: rewrite smoke suites on e2e-framework"
```

### Task 6: Rewrite the CLI suite

**Files:**
- Create: `test/e2e/suites/cli/*.go`
- Modify: `test/e2e/hack/run-cli.sh`

- [ ] **Step 1: Write a failing CLI suite bootstrap**

Use the new suite environment plus CLI support package.

- [ ] **Step 2: Run the CLI suite to verify failure**

Run:
```bash
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/cli -count=1 -v
```

Expected: FAIL

- [ ] **Step 3: Rewrite `logs` into suite style**

Move the current CLI live test into `e2e-framework` feature form with proper labels.

- [ ] **Step 4: Point `run-cli.sh` at the new suite path**

- [ ] **Step 5: Run live CLI verification**

Run:
```bash
make e2e-cli
```

Expected: PASS in the real KIND-backed environment.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/suites/cli test/e2e/hack/run-cli.sh
git commit -m "test: rewrite cli suite on e2e-framework"
```

## Chunk 4: Port Remaining Legacy Behaviors

### Task 7: Port remaining high-value validation and lifecycle scenarios

**Files:**
- Create: new files under `test/e2e/suites/basicvalidation/`
- Create: new files under `test/e2e/suites/lifecycle/`
- Create: new files under `test/e2e/suites/cli/`

- [ ] **Step 1: Port `port-validation`**

Use `test/e2e/legacy/01-basic-validation/port-validation.sh` as the specification.

- [ ] **Step 2: Run the targeted suite**

Run:
```bash
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/basicvalidation -count=1 -run PortValidation -v
```

Expected: PASS

- [ ] **Step 3: Port `env-workingdir`**

Use `test/e2e/legacy/01-basic-validation/env-workingdir.sh` as the specification.

- [ ] **Step 4: Run the targeted suite**

Run:
```bash
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/basicvalidation -count=1 -run WorkingDir -v
```

Expected: PASS

- [ ] **Step 5: Port `graceful-shutdown`**

Use `test/e2e/legacy/03-lifecycle/graceful-shutdown.sh` as the specification.

- [ ] **Step 6: Run the targeted suite**

Run:
```bash
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/lifecycle -count=1 -run GracefulShutdown -v
```

Expected: PASS

- [ ] **Step 7: Port `cli-cache` and `update-reset`**

Use the legacy shell sources as specifications.

- [ ] **Step 8: Run the CLI suite**

Run:
```bash
make e2e-cli
```

Expected: PASS with the expanded CLI coverage.

- [ ] **Step 9: Commit**

```bash
git add test/e2e/suites/basicvalidation test/e2e/suites/lifecycle test/e2e/suites/cli
git commit -m "test: port validation and cli legacy cases"
```

### Task 8: Port janitor, advanced, and recovery scenarios

**Files:**
- Create: `test/e2e/suites/janitor/*.go`
- Create: `test/e2e/suites/advanced/*.go`
- Create: `test/e2e/suites/recovery/*.go`
- Modify: `test/e2e/hack/` scripts as needed for webhook or chaos setup

- [ ] **Step 1: Port janitor scenarios**

Use:
- `legacy/04-cleanup-janitor/janitor-recovery.sh`
- `legacy/04-cleanup-janitor/namespace-aware.sh`

- [ ] **Step 2: Run the janitor suite**

Run:
```bash
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/janitor -count=1 -v
```

Expected: PASS

- [ ] **Step 3: Port advanced scenarios**

Use:
- `legacy/05-advanced-features/fast-path.sh`
- `legacy/05-advanced-features/infra-injection.sh`
- `legacy/05-advanced-features/snapshot-cleanup.sh`

- [ ] **Step 4: Run the advanced suite**

Run:
```bash
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/advanced -count=1 -v
```

Expected: PASS

- [ ] **Step 5: Port remaining recovery scenarios**

Use:
- `legacy/07-fault-recovery/controlled-recovery.sh`
- `legacy/07-fault-recovery/memory-leak.sh`
- `legacy/07-fault-recovery/pod-existence.sh`

- [ ] **Step 6: Run the recovery suite**

Run:
```bash
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/recovery -count=1 -v
```

Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add test/e2e/suites/janitor test/e2e/suites/advanced test/e2e/suites/recovery test/e2e/hack
git commit -m "test: port janitor advanced and recovery suites"
```

## Chunk 5: Remove Transitional Layout

### Task 9: Archive shell sources and delete old runtime glue

**Files:**
- Create: `test/e2e/legacy/`
- Move: `test/e2e/01-basic-validation` to `test/e2e/legacy/01-basic-validation`
- Move: `test/e2e/02-scheduling-resources` to `test/e2e/legacy/02-scheduling-resources`
- Move: `test/e2e/03-lifecycle` to `test/e2e/legacy/03-lifecycle`
- Move: `test/e2e/04-cleanup-janitor` to `test/e2e/legacy/04-cleanup-janitor`
- Move: `test/e2e/05-advanced-features` to `test/e2e/legacy/05-advanced-features`
- Move: `test/e2e/06-cli-integration` to `test/e2e/legacy/06-cli-integration`
- Move: `test/e2e/07-fault-recovery` to `test/e2e/legacy/07-fault-recovery`
- Delete: `test/e2e/common.sh`
- Delete: `test/e2e/hack/legacy_suite.sh`
- Delete: `test/e2e/framework/`
- Delete: `test/e2e/tests/`

- [ ] **Step 1: Move shell source trees into `legacy/`**

Keep them byte-for-byte intact except for path relocation.

- [ ] **Step 2: Delete transitional execution glue**

Delete:
- `test/e2e/common.sh`
- `test/e2e/hack/legacy_suite.sh`
- all old suite `test.sh` runtime wrappers
- old transitional Go support and suite directories

- [ ] **Step 3: Rewrite `test/e2e/README.md` to reflect the final state**

- [ ] **Step 4: Run final live verification**

Run:
```bash
make e2e-smoke
make e2e-cli
FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/... -count=1 -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add test/e2e
git commit -m "refactor: finalize e2e-framework test layout"
```

## Chunk 6: Final Cleanup and Documentation

### Task 10: Verify developer workflows and CI-facing entrypoints

**Files:**
- Modify: `Makefile`
- Modify: `test/e2e/README.md`
- Modify: any CI docs or scripts discovered during implementation

- [ ] **Step 1: Update `Makefile` targets**

Ensure the repository exposes clean entrypoints for:
- `e2e-smoke`
- `e2e-cli`
- future `e2e-chaos` if needed

- [ ] **Step 2: Verify no command still points at deleted transitional paths**

Run:
```bash
rg "test/e2e/(framework|tests|common\\.sh|legacy_suite)" Makefile test/e2e .github docs || true
```

Expected: no live workflow references remain.

- [ ] **Step 3: Run final verification commands**

Run:
```bash
bash -n test/e2e/hack/*.sh
go test ./test/e2e/support/... -count=1 -v
make e2e-smoke
make e2e-cli
```

Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add Makefile test/e2e/README.md docs
git commit -m "docs: finalize e2e-framework workflows"
```
