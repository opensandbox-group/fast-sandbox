# Scheduling E2E Test Migration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Migrate 02-scheduling-resources shell tests to Go e2e-framework tests.

**Architecture:** Create scheduling test suite following existing basicvalidation/lifecycle patterns. Use fixtures package for K8s operations. Single test file with shared helpers.

**Tech Stack:** Go, e2e-framework (sigs.k8s.io/e2e-framework), controller-runtime, Ginkgo/Gomega

---

## Task 1: Create Suite Entry Point

**Files:**
- Create: `test/e2e/suites/scheduling/suite_test.go`

**Step 1: Write the suite entry point**

```go
package scheduling

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

**Step 2: Verify file compiles**

Run: `cd test/e2e/suites/scheduling && go build .`
Expected: No errors (may warn about no tests)

**Step 3: Commit**

```bash
git add test/e2e/suites/scheduling/suite_test.go
git commit -m "test: add scheduling e2e suite entry point"
```

---

## Task 2: Write TestPortMutualExclusion

**Files:**
- Create: `test/e2e/suites/scheduling/scheduling_test.go`

**Step 1: Write the test with helper functions**

```go
package scheduling

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestPortMutualExclusion(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("port-mutual-exclusion").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("same-port sandboxes scheduled to different pods", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("portmutex")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createSchedulingPool(namespace, "port-mutex-pool", 2, 2, 5)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			sandboxA := createSandboxWithPorts(namespace, "sb-port-a", pool.Name, []int32{8080})
			if _, err := fixture.CreateSandbox(ctx, namespace, sandboxA); err != nil {
				t.Fatalf("create sandbox A: %v", err)
			}

			assignedA := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-port-a")
			podA := assignedA.Status.AssignedPod
			if podA == "" {
				t.Fatalf("sandbox A not assigned")
			}

			sandboxB := createSandboxWithPorts(namespace, "sb-port-b", pool.Name, []int32{8080})
			if _, err := fixture.CreateSandbox(ctx, namespace, sandboxB); err != nil {
				t.Fatalf("create sandbox B: %v", err)
			}

			assignedB := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-port-b")
			podB := assignedB.Status.AssignedPod
			if podB == "" {
				t.Fatalf("sandbox B not assigned")
			}

			if podA == podB {
				t.Fatalf("port conflict: both sandboxes on same pod %s", podA)
			}

			if len(assignedA.Status.Endpoints) > 0 {
				endpoint := assignedA.Status.Endpoints[0]
				if endpoint == "" || endpoint[len(endpoint)-5:] != ":8080" {
					t.Fatalf("unexpected endpoint: %s", endpoint)
				}
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func createSchedulingPool(namespace, name string, min, max, maxPerPod int32) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       "SandboxPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity: apiv1alpha1.PoolCapacity{
				PoolMin: min,
				PoolMax: max,
			},
			MaxSandboxesPerPod: maxPerPod,
			RuntimeType:        apiv1alpha1.RuntimeContainer,
			AgentTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: suiteenv.AgentImage(),
					}},
				},
			},
		},
	}
}

func createSandboxWithPorts(namespace, name, pool string, ports []int32) *apiv1alpha1.Sandbox {
	return &apiv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:        "docker.io/library/alpine:latest",
			Command:      []string{"/bin/sleep", "3600"},
			PoolRef:      pool,
			ExposedPorts: ports,
		},
	}
}

func waitForAssignedSandbox(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string) *apiv1alpha1.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
		return sb.Status.AssignedPod != "" &&
			(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
	})
	if err != nil {
		t.Fatalf("wait for assigned sandbox %s/%s: %v", namespace, name, err)
	}
	return sandbox
}
```

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/scheduling && go build .`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/scheduling/scheduling_test.go
git commit -m "test: add TestPortMutualExclusion for scheduling suite"
```

---

## Task 3: Write TestResourceSlotCapacity

**Files:**
- Modify: `test/e2e/suites/scheduling/scheduling_test.go`

**Step 1: Add the test function after TestPortMutualExclusion**

```go
func TestResourceSlotCapacity(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("resource-slot-capacity").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("maxSandboxesPerPod limit enforced correctly", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("slot")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createSchedulingPool(namespace, "slot-pool", 1, 1, 2)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			sandbox1 := createSandboxWithPorts(namespace, "sb-slot-1", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox1); err != nil {
				t.Fatalf("create sandbox 1: %v", err)
			}
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-slot-1")

			sandbox2 := createSandboxWithPorts(namespace, "sb-slot-2", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox2); err != nil {
				t.Fatalf("create sandbox 2: %v", err)
			}
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-slot-2")

			sandbox3 := createSandboxWithPorts(namespace, "sb-slot-3", pool.Name, nil)
			err := k8sClient.Create(ctx, sandbox3)
			if err == nil {
				t.Fatalf("sandbox 3 should be rejected due to capacity limit")
			}

			nonexistent := &apiv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "nonexistent-resource", Namespace: namespace},
			}
			if err := k8sClient.Delete(ctx, nonexistent); err == nil {
				// Success - resource didn't exist, delete is no-op
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
```

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/scheduling && go build .`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/scheduling/scheduling_test.go
git commit -m "test: add TestResourceSlotCapacity for scheduling suite"
```

---

## Task 4: Write TestAutoScaling

**Files:**
- Modify: `test/e2e/suites/scheduling/scheduling_test.go`

**Step 1: Add the test function after TestResourceSlotCapacity**

```go
func TestAutoScaling(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("auto-scaling").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("pool scales from 1 to 2 pods on demand", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("autoscale")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createSchedulingPool(namespace, "scale-pool", 1, 2, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for initial agent pod: %v", err)
			}

			sandbox1 := createSandboxWithPorts(namespace, "sb-scale-1", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox1); err != nil {
				t.Fatalf("create sandbox 1: %v", err)
			}

			sandbox2 := createSandboxWithPorts(namespace, "sb-scale-2", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox2); err != nil {
				t.Fatalf("create sandbox 2: %v", err)
			}

			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyAgentPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for pool to scale to 2 pods: %v", err)
			}

			assigned1 := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-scale-1")
			assigned2 := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-scale-2")

			if assigned1.Status.AssignedPod == assigned2.Status.AssignedPod {
				t.Fatalf("both sandboxes on same pod %s, expected different pods", assigned1.Status.AssignedPod)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
```

**Step 2: Verify compilation**

Run: `cd test/e2e/suites/scheduling && go build .`
Expected: No errors

**Step 3: Commit**

```bash
git add test/e2e/suites/scheduling/scheduling_test.go
git commit -m "test: add TestAutoScaling for scheduling suite"
```

---

## Task 5: Run Tests to Verify

**Step 1: Run the scheduling suite**

Run: `cd test/e2e/suites/scheduling && go test -v -timeout 10m`
Expected: All tests pass (requires running K8s cluster with controller)

**Step 2: If tests pass, no further action needed. If tests fail, debug and fix.**

---

## Summary

| Task | Description | Files |
|------|-------------|-------|
| 1 | Create suite entry point | `suites/scheduling/suite_test.go` |
| 2 | Write TestPortMutualExclusion | `suites/scheduling/scheduling_test.go` |
| 3 | Write TestResourceSlotCapacity | `suites/scheduling/scheduling_test.go` |
| 4 | Write TestAutoScaling | `suites/scheduling/scheduling_test.go` |
| 5 | Run and verify tests | - |