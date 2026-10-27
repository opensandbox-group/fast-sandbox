package scheduling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/fastletsettings"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"
)

func TestResourceSlotCapacity(t *testing.T) {
	suiteenv.RequireBasic(t)

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
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			sandbox1 := createSchedulingSandbox(namespace, "sb-slot-1", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox1); err != nil {
				t.Fatalf("create sandbox 1: %v", err)
			}
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-slot-1")

			sandbox2 := createSchedulingSandbox(namespace, "sb-slot-2", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox2); err != nil {
				t.Fatalf("create sandbox 2: %v", err)
			}
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-slot-2")

			sandbox3 := createSchedulingSandbox(namespace, "sb-slot-3", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox3); err != nil {
				t.Fatalf("create sandbox 3: %v", err)
			}

			// Depending on heartbeat timing, placement either filters the full
			// Fastlet immediately or Fastlet rejects a durable assignment. Both
			// paths must leave the third Sandbox Pending without a third runtime.
			pendingCtx, cancelPending := context.WithTimeout(ctx, 30*time.Second)
			defer cancelPending()
			if _, err := fixture.WaitForSandbox(pendingCtx, types.NamespacedName{Name: "sb-slot-3", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Runtime.State == apiv1alpha2.RuntimePending
			}); err != nil {
				t.Fatalf("wait for capacity-blocked Sandbox to remain Pending: %v", err)
			}
			ensureCtx, cancelEnsure := context.WithTimeout(ctx, 30*time.Second)
			defer cancelEnsure()
			if err := ensureSandboxRemainsCapacityBlocked(ensureCtx, k8sClient, types.NamespacedName{Name: "sb-slot-3", Namespace: namespace}, 10*time.Second); err != nil {
				t.Fatalf("sandbox 3 should remain Pending without consuming a third runtime slot: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func ensureSandboxRemainsCapacityBlocked(ctx context.Context, k8sClient client.Client, name types.NamespacedName, duration time.Duration) error {
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		var sandbox apiv1alpha2.Sandbox
		if err := k8sClient.Get(ctx, name, &sandbox); err != nil {
			return err
		}
		if sandbox.Status.Runtime.State == apiv1alpha2.RuntimeReady {
			return fmt.Errorf("runtime became Ready despite exhausted capacity")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil
}

func TestAutoScaling(t *testing.T) {
	suiteenv.RequireBasic(t)

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
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for initial fastlet pod: %v", err)
			}

			sandbox1 := createSchedulingSandbox(namespace, "sb-scale-1", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox1); err != nil {
				t.Fatalf("create sandbox 1: %v", err)
			}

			sandbox2 := createSchedulingSandbox(namespace, "sb-scale-2", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox2); err != nil {
				t.Fatalf("create sandbox 2: %v", err)
			}

			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for pool to scale to 2 pods: %v", err)
			}

			assigned1 := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-scale-1")
			assigned2 := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-scale-2")

			if assigned1.Status.Placement.FastletName == assigned2.Status.Placement.FastletName {
				t.Fatalf("both sandboxes on same pod %s, expected different pods", assigned1.Status.Placement.FastletName)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestResourceAwareOvercommitScaling(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-aware-overcommit-scaling").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("derived resource commitment scales out and stabilizes scale-down", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("resource-aware")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createResourceAwareSchedulingPool(namespace, "resource-aware-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create resource-aware sandbox pool: %v", err)
			}
			initialCtx, cancelInitial := context.WithTimeout(ctx, 90*time.Second)
			defer cancelInitial()
			if _, err := fixture.WaitForReadyFastletPods(initialCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for initial resource-aware fastlet: %v", err)
			}
			assertResourceAwareFastletContract(ctx, t, k8sClient, namespace, pool.Name)
			endpoint, portForward, err := e2eenv.StartControllerPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start FastPath port-forward: %v", err)
			}
			defer func() {
				if err := portForward.Cleanup(); err != nil {
					t.Errorf("cleanup FastPath port-forward: %v", err)
				}
			}()
			connection, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
			if err != nil {
				t.Fatalf("dial FastPath: %v", err)
			}
			defer connection.Close()
			fastPath := fastpathv2.NewFastPathServiceClient(connection)

			firstRequest := createResourceAwareFastPathRequest(namespace, "sb-resource-1", pool.Name, "250m", "512Mi")
			if _, err := fastPath.CreateSandbox(ctx, firstRequest); err != nil {
				t.Fatalf("create first heterogeneous Sandbox through FastPath: %v", err)
			}
			assignedFirst := waitForAssignedSandbox(ctx, t, fixture, namespace, firstRequest.RequestId)
			assertResolvedRequestAnnotations(t, pool, assignedFirst)

			secondRequest := createResourceAwareFastPathRequest(namespace, "sb-resource-2", pool.Name, "", "")
			if _, err := fastPath.CreateSandbox(ctx, secondRequest); err != nil {
				t.Fatalf("create default-profile Sandbox through FastPath: %v", err)
			}
			assignedSecond := waitForAssignedSandbox(ctx, t, fixture, namespace, secondRequest.RequestId)
			assertResolvedRequestAnnotations(t, pool, assignedSecond)
			if assignedFirst.Status.Placement.FastletName != assignedSecond.Status.Placement.FastletName {
				t.Fatalf("first two Sandboxes should fit one resource ledger, got %s and %s",
					assignedFirst.Status.Placement.FastletName, assignedSecond.Status.Placement.FastletName)
			}

			thirdRequest := createResourceAwareFastPathRequest(namespace, "sb-resource-3", pool.Name, "250m", "96Mi")
			if _, err := fastPath.CreateSandbox(ctx, thirdRequest); status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("capacity-crossing FastPath CreateSandbox code=%s err=%v, want ResourceExhausted", status.Code(err), err)
			}
			rejectedCtx, cancelRejected := context.WithTimeout(ctx, 60*time.Second)
			if err := fixture.WaitForSandboxDeleted(rejectedCtx, types.NamespacedName{Name: thirdRequest.RequestId, Namespace: namespace}); err != nil {
				cancelRejected()
				t.Fatalf("failed FastPath Create must not leave a runnable intent: %v", err)
			}
			cancelRejected()
			if err := ensureFastletPodCount(ctx, k8sClient, namespace, pool.Name, 1, 5*time.Second); err != nil {
				t.Fatalf("failed FastPath Create must not become Pool scaler demand: %v", err)
			}

			// A separately accepted declarative intent is real scaler demand. It
			// must not be confused with the failed FastPath request above.
			thirdSandbox := createSchedulingSandbox(namespace, thirdRequest.RequestId, pool.Name)
			thirdSandbox.Spec.Resources = &apiv1alpha2.SandboxResourceRequirements{Limits: apiv1alpha2.SandboxComputeResources{
				CPU: resource.MustParse("250m"), Memory: resource.MustParse("96Mi"),
			}}
			if _, err := fixture.CreateSandbox(ctx, namespace, thirdSandbox); err != nil {
				t.Fatalf("create accepted third Sandbox intent: %v", err)
			}

			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for resource commitment scale-out: %v", err)
			}

			assignments := make(map[string]apiv1alpha2.SandboxComputeResources)
			for index := 1; index <= 3; index++ {
				assigned := waitForAssignedSandbox(ctx, t, fixture, namespace, fmt.Sprintf("sb-resource-%d", index))
				assertResolvedRequestAnnotations(t, pool, assigned)
				committed := assignments[assigned.Status.Placement.FastletName]
				cpu := resource.MustParse(assigned.Annotations[assignment.AnnotationResolvedRequestCPU])
				memory := resource.MustParse(assigned.Annotations[assignment.AnnotationResolvedRequestMemory])
				committed.CPU.Add(cpu)
				committed.Memory.Add(memory)
				assignments[assigned.Status.Placement.FastletName] = committed
			}
			if len(assignments) != 2 {
				t.Fatalf("resource-aware placement used %d Fastlets, want 2: %v", len(assignments), assignments)
			}
			capacity, _, err := pool.Spec.SandboxResources.ResolveFastletCapacity(pool.Spec.MaxSandboxesPerPod)
			if err != nil {
				t.Fatalf("resolve expected Fastlet capacity: %v", err)
			}
			for fastlet, committed := range assignments {
				if committed.CPU.Cmp(capacity.CPU) > 0 || committed.Memory.Cmp(capacity.Memory) > 0 {
					t.Fatalf("Fastlet %s committed %s/%s above capacity %s/%s", fastlet,
						committed.CPU.String(), committed.Memory.String(), capacity.CPU.String(), capacity.Memory.String())
				}
			}

			for index := 1; index <= 3; index++ {
				sandbox := &apiv1alpha2.Sandbox{ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      fmt.Sprintf("sb-resource-%d", index),
				}}
				if err := k8sClient.Delete(ctx, sandbox); err != nil {
					t.Fatalf("delete resource-aware sandbox %d: %v", index, err)
				}
				deleteCtx, cancelDelete := context.WithTimeout(ctx, 60*time.Second)
				if err := fixture.WaitForSandboxDeleted(deleteCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}); err != nil {
					cancelDelete()
					t.Fatalf("wait for resource-aware sandbox %d deletion: %v", index, err)
				}
				cancelDelete()
			}

			downCtx, cancelDown := context.WithTimeout(ctx, 150*time.Second)
			defer cancelDown()
			if err := waitForFastletPodCount(downCtx, k8sClient, namespace, pool.Name, 1); err != nil {
				t.Fatalf("wait for stabilized resource-aware scale-down: %v", err)
			}
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestResourceAwareFragmentedCapacityScaling(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-aware-fragmented-capacity-scaling").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("pool scales when aggregate free resources cannot fit one Sandbox", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("resource-fragmented")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createSchedulingPool(namespace, "resource-fragmented-pool", 2, 3, 4)
			pool.Spec.SandboxResources = apiv1alpha2.NewResourceAwareSandboxResourceProfile(
				resource.MustParse("200m"), resource.MustParse("128Mi"), resource.MustParse("2"), resource.MustParse("2"), 64,
			)
			pool.Spec.Capacity.BufferMin = 0
			pool.Spec.Capacity.BufferMax = 0
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create fragmented-capacity Pool: %v", err)
			}
			initialCtx, cancelInitial := context.WithTimeout(ctx, 90*time.Second)
			defer cancelInitial()
			if _, err := fixture.WaitForReadyFastletPods(initialCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for two initial Fastlets: %v", err)
			}

			endpoint, portForward, err := e2eenv.StartControllerPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start FastPath port-forward: %v", err)
			}
			defer func() {
				if err := portForward.Cleanup(); err != nil {
					t.Errorf("cleanup FastPath port-forward: %v", err)
				}
			}()
			connection, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
			if err != nil {
				t.Fatalf("dial FastPath: %v", err)
			}
			defer connection.Close()
			fastPath := fastpathv2.NewFastPathServiceClient(connection)

			placements := make(map[string]int)
			for index := 1; index <= 6; index++ {
				name := fmt.Sprintf("sb-fragmented-%d", index)
				if _, err := fastPath.CreateSandbox(ctx, createResourceAwareFastPathRequest(namespace, name, pool.Name, "", "")); err != nil {
					t.Fatalf("create default Sandbox %d: %v", index, err)
				}
				assigned := waitForAssignedSandbox(ctx, t, fixture, namespace, name)
				placements[assigned.Status.Placement.FastletName]++
			}
			if len(placements) != 2 {
				t.Fatalf("six default Sandboxes used %d Fastlets, want 2: %v", len(placements), placements)
			}
			for fastlet, count := range placements {
				if count != 3 {
					t.Fatalf("Fastlet %s has %d default Sandboxes, want 3 before fragmentation test", fastlet, count)
				}
			}

			large := createResourceAwareFastPathRequest(namespace, "sb-fragmented-large", pool.Name, "400m", "256Mi")
			if _, err := fastPath.CreateSandbox(ctx, large); status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("fragmented FastPath CreateSandbox code=%s err=%v, want ResourceExhausted", status.Code(err), err)
			}
			rejectedCtx, cancelRejected := context.WithTimeout(ctx, 60*time.Second)
			if err := fixture.WaitForSandboxDeleted(rejectedCtx, types.NamespacedName{Name: large.RequestId, Namespace: namespace}); err != nil {
				cancelRejected()
				t.Fatalf("fragmented FastPath rejection must not leave a runnable intent: %v", err)
			}
			cancelRejected()
			largeSandbox := createSchedulingSandbox(namespace, large.RequestId, pool.Name)
			largeSandbox.Spec.Resources = &apiv1alpha2.SandboxResourceRequirements{Limits: apiv1alpha2.SandboxComputeResources{
				CPU: resource.MustParse("400m"), Memory: resource.MustParse("256Mi"),
			}}
			if _, err := fixture.CreateSandbox(ctx, namespace, largeSandbox); err != nil {
				t.Fatalf("create accepted fragmented Sandbox intent: %v", err)
			}
			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 3); err != nil {
				t.Fatalf("wait for fragmentation-driven scale-out: %v", err)
			}
			assignedLarge := waitForAssignedSandbox(ctx, t, fixture, namespace, large.RequestId)
			if _, reused := placements[assignedLarge.Status.Placement.FastletName]; reused {
				t.Fatalf("large Sandbox reused fragmented Fastlet %s instead of new capacity", assignedLarge.Status.Placement.FastletName)
			}
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestResourceAwareUnadmittableDemandIsolation(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-aware-unadmittable-demand-isolation").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("invalid Sandbox resources do not block valid Pool demand", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("resource-invalid")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createResourceAwareSchedulingPool(namespace, "resource-invalid-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create resource-aware Pool: %v", err)
			}
			initialCtx, cancelInitial := context.WithTimeout(ctx, 90*time.Second)
			defer cancelInitial()
			if _, err := fixture.WaitForReadyFastletPods(initialCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for initial Fastlet: %v", err)
			}

			// Fill the only Fastlet so the later valid Sandbox requires scale-out.
			for index := 1; index <= int(pool.Spec.MaxSandboxesPerPod); index++ {
				name := fmt.Sprintf("sb-invalid-fill-%d", index)
				sandbox := createSchedulingSandbox(namespace, name, pool.Name)
				if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
					t.Fatalf("create capacity-filling Sandbox %d: %v", index, err)
				}
				waitForAssignedSandbox(ctx, t, fixture, namespace, name)
			}

			invalid := createSchedulingSandbox(namespace, "sb-unadmittable", pool.Name)
			invalid.Spec.Resources = &apiv1alpha2.SandboxResourceRequirements{Limits: apiv1alpha2.SandboxComputeResources{
				CPU: resource.MustParse("5"), Memory: resource.MustParse("5Gi"),
			}}
			if _, err := fixture.CreateSandbox(ctx, namespace, invalid); err != nil {
				t.Fatalf("create unadmittable Sandbox intent: %v", err)
			}
			invalidCtx, cancelInvalid := context.WithTimeout(ctx, 60*time.Second)
			defer cancelInvalid()
			if _, err := fixture.WaitForSandbox(invalidCtx, types.NamespacedName{Name: invalid.Name, Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				condition := apiMeta.FindStatusCondition(sb.Status.Conditions, apiv1alpha2.SandboxConditionResourceResolved)
				return condition != nil && condition.Status == metav1.ConditionFalse &&
					condition.Reason == "LimitExceedsFastletBudget" &&
					sb.Status.Placement.FastletName == "" && sb.Status.Runtime.State == apiv1alpha2.RuntimePending
			}); err != nil {
				t.Fatalf("wait for unadmittable resource status: %v", err)
			}
			if err := ensurePoolDoesNotReportMaxReached(ctx, k8sClient, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 5*time.Second); err != nil {
				t.Fatalf("unadmittable Sandbox must not consume shared Pool capacity: %v", err)
			}

			valid := createSchedulingSandbox(namespace, "sb-admittable", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, valid); err != nil {
				t.Fatalf("create valid pending Sandbox: %v", err)
			}
			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for valid demand to scale Pool despite invalid demand: %v", err)
			}
			assigned := waitForAssignedSandbox(ctx, t, fixture, namespace, valid.Name)
			if assigned.Status.Placement.FastletName == "" {
				t.Fatal("valid Sandbox did not receive the new Fastlet capacity")
			}

			var currentInvalid apiv1alpha2.Sandbox
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: invalid.Name, Namespace: namespace}, &currentInvalid); err != nil {
				t.Fatalf("get unadmittable Sandbox after scale-out: %v", err)
			}
			condition := apiMeta.FindStatusCondition(currentInvalid.Status.Conditions, apiv1alpha2.SandboxConditionResourceResolved)
			if condition == nil || condition.Status != metav1.ConditionFalse || currentInvalid.Status.Placement.FastletName != "" {
				t.Fatalf("unadmittable Sandbox changed after valid scale-out: placement=%q condition=%+v",
					currentInvalid.Status.Placement.FastletName, condition)
			}
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestResourceAwareMemoryPressureScaling(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-aware-memory-pressure-scaling").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("blocked heartbeat removes effective capacity and rejects new placement", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("resource-pressure")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createResourceAwareSchedulingPool(namespace, "resource-pressure-pool")
			pool.Spec.Capacity.BufferMin = 1
			pool.Spec.Capacity.BufferMax = 1
			pool.Spec.FastletTemplate.Spec.Containers[0].Env = []corev1.EnvVar{
				{Name: "FAST_SANDBOX_MEMORY_OBSERVER_INTERVAL", Value: "250ms"},
				{Name: "FAST_SANDBOX_MEMORY_HIGH_WATERMARK", Value: "0.01"},
				{Name: "FAST_SANDBOX_MEMORY_LOW_WATERMARK", Value: "0.005"},
				{Name: "FAST_SANDBOX_MEMORY_HIGH_SAMPLES", Value: "1"},
				{Name: "FAST_SANDBOX_MEMORY_RECOVERY_SAMPLES", Value: "1"},
				{Name: "FAST_SANDBOX_MEMORY_PSI_AVG10_THRESHOLD", Value: "0"},
				{Name: "FAST_SANDBOX_NODE_MEMORY_LOW_WATERMARK", Value: "0"},
				{Name: "FAST_SANDBOX_NODE_MEMORY_RECOVERY_WATERMARK", Value: "0"},
			}
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create pressured resource-aware pool: %v", err)
			}

			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for pressure-driven replacement capacity: %v", err)
			}
			var pods corev1.PodList
			if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool.Name}); err != nil {
				t.Fatalf("list pressured Fastlets: %v", err)
			}
			if len(pods.Items) != 2 {
				t.Fatalf("pressured Fastlet count=%d, want 2", len(pods.Items))
			}
			for index := range pods.Items {
				waitForFastletAdmission(ctx, t, pods.Items[index], fastletapi.AdmissionStateBlocked, fastletapi.AdmissionReasonFastletMemoryHighWatermark)
			}
			conditionCtx, cancelCondition := context.WithTimeout(ctx, 90*time.Second)
			defer cancelCondition()
			conditionPool, err := fixture.WaitForPoolCondition(conditionCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, apiv1alpha2.PoolConditionCapacityAvailable, metav1.ConditionFalse)
			if err != nil {
				t.Fatalf("wait for PoolMaxReached under memory pressure: %v", err)
			}
			condition := apiMeta.FindStatusCondition(conditionPool.Status.Conditions, apiv1alpha2.PoolConditionCapacityAvailable)
			if condition == nil || condition.Reason != apiv1alpha2.ReasonPoolMaxReached {
				t.Fatalf("CapacityAvailable condition=%+v, want PoolMaxReached", condition)
			}

			sandbox := createSchedulingSandbox(namespace, "sb-pressure-blocked", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create Sandbox under memory pressure: %v", err)
			}
			if err := fixture.EnsureSandboxRemainsUnassigned(ctx,
				types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, 10*time.Second,
			); err != nil {
				t.Fatalf("Sandbox should remain unassigned while all Fastlets are memory-blocked: %v", err)
			}
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestResourceAwareBufferUsesSandboxSlots(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-aware-buffer-sandbox-slots").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("buffer thresholds retain default Sandbox commitments, not Fastlet Pods", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("resource-buffer")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createResourceAwareSchedulingPool(namespace, "resource-buffer-pool")
			pool.Spec.Capacity.BufferMin = 2
			pool.Spec.Capacity.BufferMax = 2
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create resource-aware Pool: %v", err)
			}
			initialCtx, cancelInitial := context.WithTimeout(ctx, 90*time.Second)
			defer cancelInitial()
			if _, err := fixture.WaitForReadyFastletPods(initialCtx, client.ObjectKeyFromObject(pool), 1); err != nil {
				t.Fatalf("wait for initial Fastlet: %v", err)
			}
			if err := ensureFastletPodCount(ctx, k8sClient, namespace, pool.Name, 1, 5*time.Second); err != nil {
				t.Fatalf("two free buffer slots must not mean two spare Fastlets: %v", err)
			}

			for index := 1; index <= 3; index++ {
				name := fmt.Sprintf("sb-buffer-%d", index)
				if _, err := fixture.CreateSandbox(ctx, namespace, createSchedulingSandbox(namespace, name, pool.Name)); err != nil {
					t.Fatalf("create Sandbox %d: %v", index, err)
				}
				waitForAssignedSandbox(ctx, t, fixture, namespace, name)
			}
			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, client.ObjectKeyFromObject(pool), 2); err != nil {
				t.Fatalf("wait for buffer-slot scale-out: %v", err)
			}
			return ctx
		}).Feature()

	testSuite.Env().Test(t, feature)
}

func TestResourceAwareHostPressurePropagatesAcrossPools(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-aware-host-pressure-cross-pool").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "validation").
		Assess("host pressure reported by one Fastlet blocks same-node capacity in another Pool", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("resource-host-pressure")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)
			nodeName := firstSchedulableNode(ctx, t, k8sClient)

			source := createResourceAwareSchedulingPool(namespace, "host-pressure-source")
			source.Spec.Capacity = apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1}
			source.Spec.FastletTemplate.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
			source.Spec.FastletTemplate.Spec.Containers[0].Env = resourceObserverEnv("1", "0.99", "1", "1")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, source); err != nil {
				t.Fatalf("create host-pressure source Pool: %v", err)
			}
			target := createResourceAwareSchedulingPool(namespace, "host-pressure-target")
			target.Spec.Capacity = apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 2}
			target.Spec.FastletTemplate.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
			target.Spec.FastletTemplate.Spec.Containers[0].Env = resourceObserverEnv("1", "0.99", "0", "0")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, target); err != nil {
				t.Fatalf("create host-pressure target Pool: %v", err)
			}
			readyCtx, cancelReady := context.WithTimeout(ctx, 90*time.Second)
			defer cancelReady()
			if _, err := fixture.WaitForReadyFastletPods(readyCtx, client.ObjectKeyFromObject(source), 1); err != nil {
				t.Fatalf("wait for source Fastlet: %v", err)
			}
			if _, err := fixture.WaitForReadyFastletPods(readyCtx, client.ObjectKeyFromObject(target), 1); err != nil {
				t.Fatalf("wait for target Fastlet: %v", err)
			}
			sourcePod := onlyPoolPod(ctx, t, k8sClient, namespace, source.Name)
			targetPod := onlyPoolPod(ctx, t, k8sClient, namespace, target.Name)
			waitForFastletAdmission(ctx, t, sourcePod, fastletapi.AdmissionStateBlocked, fastletapi.AdmissionReasonHostMemoryPressure)
			waitForFastletAdmission(ctx, t, targetPod, fastletapi.AdmissionStateOpen, "")

			sandbox := createSchedulingSandbox(namespace, "sb-host-pressure", target.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create target Sandbox: %v", err)
			}
			scaleCtx, cancelScale := context.WithTimeout(ctx, 150*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, client.ObjectKeyFromObject(target), 2); err != nil {
				t.Fatalf("wait for target Pool scale-out under host pressure: %v", err)
			}
			if err := fixture.EnsureSandboxRemainsUnassigned(ctx, client.ObjectKeyFromObject(sandbox), 10*time.Second); err != nil {
				t.Fatalf("Sandbox should remain unassigned while its node is pressure-blocked: %v", err)
			}
			conditionPool, err := fixture.WaitForPoolCondition(scaleCtx, client.ObjectKeyFromObject(target), apiv1alpha2.PoolConditionCapacityAvailable, metav1.ConditionFalse)
			if err != nil {
				t.Fatalf("wait for target PoolMaxReached: %v", err)
			}
			condition := apiMeta.FindStatusCondition(conditionPool.Status.Conditions, apiv1alpha2.PoolConditionCapacityAvailable)
			if condition == nil || condition.Reason != apiv1alpha2.ReasonPoolMaxReached {
				t.Fatalf("target CapacityAvailable condition=%+v, want PoolMaxReached", condition)
			}
			return ctx
		}).Feature()

	testSuite.Env().Test(t, feature)
}

func createResourceAwareSchedulingPool(namespace, name string) *apiv1alpha2.SandboxPool {
	pool := createSchedulingPool(namespace, name, 1, 2, 4)
	pool.Spec.SandboxResources = apiv1alpha2.NewResourceAwareSandboxResourceProfile(
		resource.MustParse("500m"), resource.MustParse("192Mi"), resource.MustParse("5"), resource.MustParse("1.5"), 64,
	)
	pool.Spec.FastletTemplate.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "FAST_SANDBOX_MEMORY_HIGH_WATERMARK", Value: "0.99"},
		{Name: "FAST_SANDBOX_MEMORY_LOW_WATERMARK", Value: "0.95"},
		{Name: "FAST_SANDBOX_MEMORY_PSI_AVG10_THRESHOLD", Value: "0"},
		{Name: "FAST_SANDBOX_NODE_MEMORY_LOW_WATERMARK", Value: "0"},
		{Name: "FAST_SANDBOX_NODE_MEMORY_RECOVERY_WATERMARK", Value: "0"},
	}
	return pool
}

func assertResourceAwareFastletContract(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, pool string) {
	t.Helper()
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"fast-sandbox.io/pool": pool},
	); err != nil {
		t.Fatalf("list resource-aware Fastlets: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("initial resource-aware Fastlet count=%d, want 1", len(pods.Items))
	}

	fastlet := pods.Items[0].Spec.Containers[0]
	wantCPU := resource.MustParse("600m")
	wantMemory := resource.MustParse("640Mi")
	for name, resources := range map[string]corev1.ResourceList{
		"requests": fastlet.Resources.Requests,
		"limits":   fastlet.Resources.Limits,
	} {
		if got := resources[corev1.ResourceCPU]; got.Cmp(wantCPU) != 0 {
			t.Fatalf("Fastlet %s CPU=%s, want %s", name, got.String(), wantCPU.String())
		}
		if got := resources[corev1.ResourceMemory]; got.Cmp(wantMemory) != 0 {
			t.Fatalf("Fastlet %s memory=%s, want %s", name, got.String(), wantMemory.String())
		}
	}

	capacity, err := fastletsettings.DecodeCommitmentCapacity(pods.Items[0].Annotations[fastletsettings.CommitmentCapacityAnnotation])
	if err != nil {
		t.Fatalf("decode Fastlet commitment capacity: %v", err)
	}
	wantCapacityCPU := resource.MustParse("400m")
	wantCapacityMemory := resource.MustParse("512Mi")
	if capacity.CPU.Cmp(wantCapacityCPU) != 0 || capacity.Memory.Cmp(wantCapacityMemory) != 0 {
		t.Fatalf("Fastlet commitment capacity=(%s,%s), want (%s,%s)",
			capacity.CPU.String(), capacity.Memory.String(), wantCapacityCPU.String(), wantCapacityMemory.String())
	}
}

func createResourceAwareFastPathRequest(namespace, name, pool, cpuLimit, memoryLimit string) *fastpathv2.CreateSandboxRequest {
	request := &fastpathv2.CreateSandboxRequest{
		RequestId: name,
		Namespace: namespace,
		Image:     "docker.io/library/alpine:latest",
		PoolRef:   pool,
		Command:   []string{"/bin/sleep", "3600"},
	}
	if cpuLimit != "" || memoryLimit != "" {
		request.Resources = &fastpathv2.SandboxResourceRequirements{
			Limits: &fastpathv2.ComputeResourceProfile{Cpu: cpuLimit, Memory: memoryLimit},
		}
	}
	return request
}

func assertResolvedRequestAnnotations(t *testing.T, pool *apiv1alpha2.SandboxPool, sandbox *apiv1alpha2.Sandbox) {
	t.Helper()
	limits, err := pool.Spec.SandboxResources.ResolveLimits(sandbox.Spec.Resources)
	if err != nil {
		t.Fatalf("resolve expected limits for Sandbox %s: %v", sandbox.Name, err)
	}
	want, err := pool.Spec.SandboxResources.DeriveRequests(limits)
	if err != nil {
		t.Fatalf("derive expected requests for Sandbox %s: %v", sandbox.Name, err)
	}
	gotCPU, err := resource.ParseQuantity(sandbox.Annotations[assignment.AnnotationResolvedRequestCPU])
	if err != nil {
		t.Fatalf("parse resolved CPU request annotation for Sandbox %s: %v", sandbox.Name, err)
	}
	gotMemory, err := resource.ParseQuantity(sandbox.Annotations[assignment.AnnotationResolvedRequestMemory])
	if err != nil {
		t.Fatalf("parse resolved memory request annotation for Sandbox %s: %v", sandbox.Name, err)
	}
	if gotCPU.Cmp(want.CPU) != 0 || gotMemory.Cmp(want.Memory) != 0 {
		t.Fatalf("Sandbox %s resolved requests=%s/%s, want %s/%s", sandbox.Name,
			gotCPU.String(), gotMemory.String(), want.CPU.String(), want.Memory.String())
	}
	if got := sandbox.Annotations[assignment.AnnotationResourcePolicyHash]; got != pool.Spec.SandboxResources.Hash() {
		t.Fatalf("Sandbox %s resource policy hash=%q, want %q", sandbox.Name, got, pool.Spec.SandboxResources.Hash())
	}
	gotLimitCPU, err := resource.ParseQuantity(sandbox.Annotations[assignment.AnnotationResolvedLimitCPU])
	if err != nil {
		t.Fatalf("parse resolved CPU limit annotation for Sandbox %s: %v", sandbox.Name, err)
	}
	gotLimitMemory, err := resource.ParseQuantity(sandbox.Annotations[assignment.AnnotationResolvedLimitMemory])
	if err != nil {
		t.Fatalf("parse resolved memory limit annotation for Sandbox %s: %v", sandbox.Name, err)
	}
	if gotLimitCPU.Cmp(limits.CPU) != 0 || gotLimitMemory.Cmp(limits.Memory) != 0 {
		t.Fatalf("Sandbox %s resolved limits=%s/%s, want %s/%s", sandbox.Name,
			gotLimitCPU.String(), gotLimitMemory.String(), limits.CPU.String(), limits.Memory.String())
	}
}

func waitForFastletPodCount(ctx context.Context, k8sClient client.Client, namespace, pool string, want int) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		pods := &corev1.PodList{}
		if err := k8sClient.List(ctx, pods,
			client.InNamespace(namespace),
			client.MatchingLabels{"fast-sandbox.io/pool": pool},
		); err == nil && len(pods.Items) == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func ensureFastletPodCount(ctx context.Context, k8sClient client.Client, namespace, pool string, want int, duration time.Duration) error {
	checkCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := checkCtx.Err(); err != nil {
			if err == context.DeadlineExceeded {
				return nil
			}
			return err
		}
		pods := &corev1.PodList{}
		if err := k8sClient.List(checkCtx, pods,
			client.InNamespace(namespace),
			client.MatchingLabels{"fast-sandbox.io/pool": pool},
		); err != nil {
			if checkCtx.Err() == context.DeadlineExceeded {
				return nil
			}
			return err
		}
		if len(pods.Items) != want {
			return fmt.Errorf("Fastlet Pod count=%d, want %d throughout %s", len(pods.Items), want, duration)
		}
		select {
		case <-checkCtx.Done():
			if checkCtx.Err() == context.DeadlineExceeded {
				return nil
			}
			return checkCtx.Err()
		case <-ticker.C:
		}
	}
}

func ensurePoolDoesNotReportMaxReached(ctx context.Context, k8sClient client.Client, name types.NamespacedName, duration time.Duration) error {
	checkCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := checkCtx.Err(); err != nil {
			if err == context.DeadlineExceeded {
				return nil
			}
			return err
		}
		var pool apiv1alpha2.SandboxPool
		if err := k8sClient.Get(checkCtx, name, &pool); err != nil {
			if checkCtx.Err() == context.DeadlineExceeded {
				return nil
			}
			return err
		}
		condition := apiMeta.FindStatusCondition(pool.Status.Conditions, apiv1alpha2.PoolConditionCapacityAvailable)
		if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == apiv1alpha2.ReasonPoolMaxReached {
			return fmt.Errorf("Pool reported PoolMaxReached for an unadmittable Sandbox: %s", condition.Message)
		}
		select {
		case <-checkCtx.Done():
			if checkCtx.Err() == context.DeadlineExceeded {
				return nil
			}
			return checkCtx.Err()
		case <-ticker.C:
		}
	}
}

func waitForFastletAdmission(ctx context.Context, t *testing.T, pod corev1.Pod, state fastletapi.AdmissionState, reason fastletapi.AdmissionReason) *fastletapi.ResourceAdmissionStatus {
	t.Helper()
	endpoint, forward, err := e2eenv.StartPodPortForward(ctx, pod.Namespace, pod.Name, 5758)
	if err != nil {
		t.Fatalf("start heartbeat port-forward for %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	defer func() {
		if err := forward.Cleanup(); err != nil {
			t.Errorf("cleanup heartbeat port-forward for %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last *fastletapi.ResourceAdmissionStatus
	for {
		request, requestErr := http.NewRequestWithContext(waitCtx, http.MethodGet, endpoint+"/api/v2/fastlet/heartbeat", nil)
		if requestErr != nil {
			t.Fatalf("build heartbeat request for %s/%s: %v", pod.Namespace, pod.Name, requestErr)
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			var heartbeat fastletapi.HeartbeatResponse
			decodeErr := json.NewDecoder(response.Body).Decode(&heartbeat)
			_ = response.Body.Close()
			if decodeErr == nil {
				last = heartbeat.Admission.Resources
				if last != nil && last.State == state && (reason == "" || last.Reason == reason) {
					return last
				}
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for Fastlet %s/%s admission state=%s reason=%s (last=%+v): %v", pod.Namespace, pod.Name, state, reason, last, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func resourceObserverEnv(localHigh, localLow, hostLow, hostRecovery string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "FAST_SANDBOX_MEMORY_OBSERVER_INTERVAL", Value: "250ms"},
		{Name: "FAST_SANDBOX_MEMORY_HIGH_WATERMARK", Value: localHigh},
		{Name: "FAST_SANDBOX_MEMORY_LOW_WATERMARK", Value: localLow},
		{Name: "FAST_SANDBOX_MEMORY_HIGH_SAMPLES", Value: "1"},
		{Name: "FAST_SANDBOX_MEMORY_RECOVERY_SAMPLES", Value: "1"},
		{Name: "FAST_SANDBOX_MEMORY_PSI_AVG10_THRESHOLD", Value: "0"},
		{Name: "FAST_SANDBOX_NODE_MEMORY_LOW_WATERMARK", Value: hostLow},
		{Name: "FAST_SANDBOX_NODE_MEMORY_RECOVERY_WATERMARK", Value: hostRecovery},
	}
}

func firstSchedulableNode(ctx context.Context, t *testing.T, k8sClient client.Client) string {
	t.Helper()
	var nodes corev1.NodeList
	if err := k8sClient.List(ctx, &nodes); err != nil {
		t.Fatalf("list Nodes: %v", err)
	}
	for index := range nodes.Items {
		node := &nodes.Items[index]
		if node.Spec.Unschedulable {
			continue
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				return node.Name
			}
		}
	}
	t.Fatal("no schedulable Ready Node found")
	return ""
}

func onlyPoolPod(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, pool string) corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool}); err != nil {
		t.Fatalf("list Fastlets for Pool %s/%s: %v", namespace, pool, err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("Fastlet count for Pool %s/%s=%d, want 1", namespace, pool, len(pods.Items))
	}
	return pods.Items[0]
}

func createSchedulingPool(namespace, name string, min, max, maxPerPod int32) *apiv1alpha2.SandboxPool {
	return &apiv1alpha2.SandboxPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha2.GroupVersion.String(),
			Kind:       "SandboxPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Capacity: apiv1alpha2.PoolCapacity{
				PoolMin: min,
				PoolMax: max,
			},
			MaxSandboxesPerPod: maxPerPod,
			Runtime:            apiv1alpha2.RuntimeContainer,
			SandboxResources:   suiteenv.SmallSandboxResourceProfile(),
			FastletTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "fastlet",
						Image: suiteenv.FastletImage(),
					}},
				},
			},
		},
	}
}

func createSchedulingSandbox(namespace, name, pool string) *apiv1alpha2.Sandbox {
	return &apiv1alpha2.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha2.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha2.SandboxSpec{
			Image:   "docker.io/library/alpine:latest",
			Command: []string{"/bin/sleep", "3600"},
			PoolRef: pool,
		},
	}
}

func waitForAssignedSandbox(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string) *apiv1alpha2.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: name, Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
		return sb.Status.Placement.FastletName != "" && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
	})
	if err != nil {
		t.Fatalf("wait for assigned sandbox %s/%s: %v", namespace, name, err)
	}
	return sandbox
}
