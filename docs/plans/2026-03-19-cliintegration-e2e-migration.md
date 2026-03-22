# CLI Integration E2E Test Migration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Migrate 06-cli-integration shell tests to Go e2e-framework tests.

**Architecture:** Create cliintegration test suite following existing patterns. Tests verify fsb-ctl CLI commands (update, reset, logs, run) work correctly against the controller.

**Tech Stack:** Go, e2e-framework (sigs.k8s.io/e2e-framework), controller-runtime, fsb-ctl CLI

---

## Task 1: Create Suite Entry Point

**Files:**
- Create: `test/e2e/suites/cliintegration/suite_test.go`

```go
package cliintegration

import (
	"os"
	"testing"

	"fast-sandbox/test/e2e/support/suiteenv"
)

var testSuite = suiteenv.New()

func TestMain(m *testing.M) {
	os.Exit(testSuite.Env().Run(m))
}
```

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/cliintegration && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/cliintegration/suite_test.go
git commit -m "test: add cliintegration e2e suite entry point"
```

---

## Task 2: Write TestUpdateReset

**Files:**
- Create: `test/e2e/suites/cliintegration/cliintegration_test.go`

Test validates fsb-ctl update and reset commands:
1. Build fsb-ctl binary if needed
2. Create sandbox pool and wait for agent pods
3. Start port-forward to controller gRPC endpoint
4. Create sandbox using kubectl
5. Test fsb-ctl get command
6. Test fsb-ctl update --labels command
7. Test fsb-ctl reset command
8. Verify ResetRevision was set

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/cliintegration && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/cliintegration/cliintegration_test.go
git commit -m "test: add TestUpdateReset for cliintegration suite"
```

---

## Task 3: Write TestCLILogs

**Files:**
- Modify: `test/e2e/suites/cliintegration/cliintegration_test.go`

Test validates fsb-ctl logs command:
1. Create sandbox that produces log output
2. Wait for sandbox to be running
3. Test fsb-ctl logs command to retrieve logs
4. Verify expected log content appears

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/cliintegration && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/cliintegration/cliintegration_test.go
git commit -m "test: add TestCLILogs for cliintegration suite"
```

---

## Task 4: Write TestCLIRun

**Files:**
- Modify: `test/e2e/suites/cliintegration/cliintegration_test.go`

Test validates fsb-ctl run command:
1. Create a YAML config file for sandbox spec
2. Test fsb-ctl run with config file
3. Verify sandbox is created successfully

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/cliintegration && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/cliintegration/cliintegration_test.go
git commit -m "test: add TestCLIRun for cliintegration suite"
```

---

## Summary

| Task | Description | Files |
|------|-------------|-------|
| 1 | Create suite entry point | `suites/cliintegration/suite_test.go` |
| 2 | Write TestUpdateReset | `suites/cliintegration/cliintegration_test.go` |
| 3 | Write TestCLILogs | `suites/cliintegration/cliintegration_test.go` |
| 4 | Write TestCLIRun | `suites/cliintegration/cliintegration_test.go` |