# Fault Recovery E2E Test Migration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Migrate 07-fault-recovery shell tests to Go e2e-framework tests.

**Architecture:** Create faultrecovery test suite following existing patterns. Tests verify auto-expiry, memory leak prevention, controlled recovery, and pod existence checks.

**Tech Stack:** Go, e2e-framework (sigs.k8s.io/e2e-framework), controller-runtime

---

## Task 1: Create Suite Entry Point

**Files:**
- Create: `test/e2e/suites/faultrecovery/suite_test.go`

```go
package faultrecovery

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

Run: `cd test/e2e/suites/faultrecovery && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/faultrecovery/suite_test.go
git commit -m "test: add faultrecovery e2e suite entry point"
```

---

## Task 2: Write TestAutoExpiry

**Files:**
- Create: `test/e2e/suites/faultrecovery/faultrecovery_test.go`

Test validates sandbox with expireTime is garbage collected:
1. Create sandbox with 20-second expiry
2. Wait for expiry
3. Verify phase becomes Expired
4. Verify CRD is preserved, assignedPod and sandboxID cleared

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/faultrecovery && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/faultrecovery/faultrecovery_test.go
git commit -m "test: add TestAutoExpiry for faultrecovery suite"
```

---

## Task 3: Write TestMemoryLeak

**Files:**
- Modify: `test/e2e/suites/faultrecovery/faultrecovery_test.go`

Test validates registry handles create/delete cycles:
1. Create 5 sandboxes
2. Delete 3 sandboxes
3. Create new sandbox to verify registry works
4. Create more sandboxes to verify continued functionality

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/faultrecovery && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/faultrecovery/faultrecovery_test.go
git commit -m "test: add TestMemoryLeak for faultrecovery suite"
```

---

## Task 4: Write TestControlledRecovery

**Files:**
- Modify: `test/e2e/suites/faultrecovery/faultrecovery_test.go`

Test validates manual reset and auto-recreate:
1. Manual reset via ResetRevision
2. Verify reset is accepted by controller
3. Set AutoRecreate policy
4. Delete agent pod to trigger disconnect
5. Verify sandbox is rescheduled to new pod

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/faultrecovery && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/faultrecovery/faultrecovery_test.go
git commit -m "test: add TestControlledRecovery for faultrecovery suite"
```

---

## Task 5: Write TestPodExistence

**Files:**
- Modify: `test/e2e/suites/faultrecovery/faultrecovery_test.go`

Test validates janitor correctly identifies orphan containers:
1. Create sandbox with exposed ports
2. Delete agent pod to simulate orphan
3. Wait for janitor scan cycle
4. Verify sandbox state reflects orphan handling

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/faultrecovery && go test -c`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/faultrecovery/faultrecovery_test.go
git commit -m "test: add TestPodExistence for faultrecovery suite"
```

---

## Summary

| Task | Description | Files |
|------|-------------|-------|
| 1 | Create suite entry point | `suites/faultrecovery/suite_test.go` |
| 2 | Write TestAutoExpiry | `suites/faultrecovery/faultrecovery_test.go` |
| 3 | Write TestMemoryLeak | `suites/faultrecovery/faultrecovery_test.go` |
| 4 | Write TestControlledRecovery | `suites/faultrecovery/faultrecovery_test.go` |
| 5 | Write TestPodExistence | `suites/faultrecovery/faultrecovery_test.go` |