package controlplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/suiteenv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	controlPlaneNamespace = "default"
	leaderLeaseName       = "fast-sandbox-controller.sandbox.fast.io"
)

func TestMultiActiveControlPlane(t *testing.T) {
	suiteenv.RequireBasic(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	k8sClient := testSuite.MustKubeClient(t)

	assertProductionTopology(ctx, t, k8sClient)

	namespace := testSuite.AllocateNamespace("ha")
	createNamespace(ctx, t, k8sClient, namespace)
	defer suiteenv.DeleteNamespace(context.Background(), t, k8sClient, namespace)

	pool := controlPlanePool(namespace, "ha-pool", 10)
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create HA pool: %v", err)
	}
	waitForReadyFastlets(ctx, t, k8sClient, namespace, pool.Name, 1)

	endpoint, portForward, err := e2eenv.StartControllerPortForward(ctx, controlPlaneNamespace)
	if err != nil {
		t.Fatalf("start FastPath port-forward: %v", err)
	}
	defer func() {
		if err := portForward.Cleanup(); err != nil {
			t.Errorf("cleanup FastPath port-forward: %v", err)
		}
	}()
	conn, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial FastPath: %v", err)
	}
	defer conn.Close()
	fastPath := fastpathv1.NewFastPathServiceClient(conn)

	t.Run("direct CRD creation is reconciled independently of FastPath", func(t *testing.T) {
		sandbox := &apiv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "declarative", Namespace: namespace},
			Spec: apiv1alpha1.SandboxSpec{
				Image: "docker.io/library/alpine:latest", PoolRef: pool.Name,
				Command: []string{"/bin/sh", "-c", "sleep 3600"},
			},
		}
		if err := k8sClient.Create(ctx, sandbox); err != nil {
			t.Fatalf("create declarative Sandbox: %v", err)
		}
		ready := waitForSandbox(ctx, t, k8sClient, types.NamespacedName{Namespace: namespace, Name: sandbox.Name}, func(item *apiv1alpha1.Sandbox) bool {
			return item.Status.Assignment != nil && item.Status.RuntimeState == apiv1alpha1.ObservedStateReady
		})
		if ready.Status.SandboxID != string(ready.UID) {
			t.Fatalf("Sandbox ID must be durable UID: id=%q uid=%q", ready.Status.SandboxID, ready.UID)
		}
	})

	t.Run("request ID is idempotent and spec-bound", func(t *testing.T) {
		requestID := namespace + "-idempotent"
		request := createRequest(namespace, pool.Name, requestID)
		first, err := fastPath.CreateSandbox(ctx, request)
		if err != nil {
			t.Fatalf("first CreateSandbox: %v", err)
		}
		second, err := fastPath.CreateSandbox(ctx, createRequest(namespace, pool.Name, requestID))
		if err != nil {
			t.Fatalf("idempotent CreateSandbox: %v", err)
		}
		if first.SandboxUid == "" || first.SandboxUid != second.SandboxUid || first.SandboxName != second.SandboxName {
			t.Fatalf("idempotent response mismatch: first=%+v second=%+v", first, second)
		}
		conflict := createRequest(namespace, pool.Name, requestID)
		conflict.Image = "docker.io/library/busybox:latest"
		if _, err := fastPath.CreateSandbox(ctx, conflict); status.Code(err) != codes.AlreadyExists {
			t.Fatalf("same request ID with different spec: code=%s err=%v", status.Code(err), err)
		}
	})

	t.Run("FastPath remains available while Controller leader changes", func(t *testing.T) {
		before := currentLeader(ctx, t, k8sClient)
		leaderPod := strings.SplitN(before, "_", 2)[0]
		if leaderPod == "" {
			t.Fatalf("invalid leader identity %q", before)
		}
		zero := int64(0)
		if err := k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: leaderPod, Namespace: controlPlaneNamespace}}, &client.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
			t.Fatalf("delete Controller leader %s: %v", leaderPod, err)
		}

		requestCtx, requestCancel := context.WithTimeout(ctx, 20*time.Second)
		defer requestCancel()
		response, err := fastPath.CreateSandbox(requestCtx, createRequest(namespace, pool.Name, namespace+"-during-election"))
		if err != nil {
			t.Fatalf("FastPath Create during Controller election: %v", err)
		}
		if response.SandboxUid == "" {
			t.Fatal("FastPath returned empty Sandbox UID during Controller election")
		}

		waitUntil(ctx, t, "new Controller leader", func() (bool, error) {
			var lease coordinationv1.Lease
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: controlPlaneNamespace, Name: leaderLeaseName}, &lease); err != nil {
				return false, err
			}
			return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" && *lease.Spec.HolderIdentity != before, nil
		})
		waitForDeploymentReady(ctx, t, k8sClient, "fast-sandbox-controller", 2)
	})

	t.Run("concurrent RPC admission never exceeds Fastlet capacity", func(t *testing.T) {
		capacityPool := controlPlanePool(namespace, "capacity-pool", 3)
		if err := k8sClient.Create(ctx, capacityPool); err != nil {
			t.Fatalf("create capacity pool: %v", err)
		}
		waitForReadyFastlets(ctx, t, k8sClient, namespace, capacityPool.Name, 1)

		const requests = 40
		var group sync.WaitGroup
		var lock sync.Mutex
		successes := 0
		failures := make([]error, 0, requests)
		for index := 0; index < requests; index++ {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				requestCtx, requestCancel := context.WithTimeout(ctx, 30*time.Second)
				defer requestCancel()
				_, createErr := fastPath.CreateSandbox(requestCtx, createRequest(namespace, capacityPool.Name, fmt.Sprintf("%s-capacity-%d", namespace, index)))
				lock.Lock()
				defer lock.Unlock()
				if createErr == nil {
					successes++
					return
				}
				failures = append(failures, createErr)
			}(index)
		}
		group.Wait()
		if successes != int(capacityPool.Spec.MaxSandboxesPerPod) {
			t.Fatalf("successful admissions=%d, want capacity=%d; failures=%v", successes, capacityPool.Spec.MaxSandboxesPerPod, failures)
		}
		for _, failure := range failures {
			if status.Code(failure) != codes.ResourceExhausted {
				t.Fatalf("capacity failure code=%s, want ResourceExhausted: %v", status.Code(failure), failure)
			}
		}
		waitUntil(ctx, t, "only admitted capacity CRDs", func() (bool, error) {
			var list apiv1alpha1.SandboxList
			if err := k8sClient.List(ctx, &list, client.InNamespace(namespace)); err != nil {
				return false, err
			}
			count := 0
			for index := range list.Items {
				if list.Items[index].Spec.PoolRef == capacityPool.Name {
					count++
				}
			}
			return count == successes, nil
		})
	})
}

func assertProductionTopology(ctx context.Context, t *testing.T, k8sClient client.Client) {
	t.Helper()
	waitForDeploymentReady(ctx, t, k8sClient, "fast-sandbox-controller", 2)
	waitForDeploymentReady(ctx, t, k8sClient, "fast-sandbox-fastpath", 3)

	var controller appsv1.Deployment
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: controlPlaneNamespace, Name: "fast-sandbox-controller"}, &controller); err != nil {
		t.Fatalf("get Controller Deployment: %v", err)
	}
	if !contains(controller.Spec.Template.Spec.Containers[0].Args, "--role=controller") {
		t.Fatalf("Controller args do not select controller role: %v", controller.Spec.Template.Spec.Containers[0].Args)
	}
	var fastPath appsv1.Deployment
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: controlPlaneNamespace, Name: "fast-sandbox-fastpath"}, &fastPath); err != nil {
		t.Fatalf("get FastPath Deployment: %v", err)
	}
	if !contains(fastPath.Spec.Template.Spec.Containers[0].Args, "--role=fastpath") {
		t.Fatalf("FastPath args do not select fastpath role: %v", fastPath.Spec.Template.Spec.Containers[0].Args)
	}

	var endpoints corev1.Endpoints
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: controlPlaneNamespace, Name: "fast-sandbox-fastpath"}, &endpoints); err != nil {
		t.Fatalf("get FastPath Service endpoints: %v", err)
	}
	addressCount := 0
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			addressCount++
			if address.TargetRef == nil || !strings.HasPrefix(address.TargetRef.Name, "fast-sandbox-fastpath-") {
				t.Fatalf("FastPath Service selected non-FastPath endpoint: %+v", address)
			}
		}
	}
	if addressCount != 3 {
		t.Fatalf("FastPath endpoint count=%d, want 3", addressCount)
	}
	_ = currentLeader(ctx, t, k8sClient)

	for _, object := range []client.Object{
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "fast-sandbox-controller", Namespace: controlPlaneNamespace}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "fast-sandbox-fastpath", Namespace: controlPlaneNamespace}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "fast-sandbox-fastpath", Namespace: controlPlaneNamespace}},
	} {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatalf("get topology object %T/%s: %v", object, object.GetName(), err)
		}
	}
}

func controlPlanePool(namespace, name string, capacity int32) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity:           apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 1},
			MaxSandboxesPerPod: capacity,
			Runtime:            apiv1alpha1.RuntimeContainer,
			SandboxResources: apiv1alpha1.SandboxResourceProfile{
				CPU: resource.MustParse("50m"), Memory: resource.MustParse("64Mi"), PIDs: 64,
			},
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "fastlet", Image: suiteenv.FastletImage(), ImagePullPolicy: corev1.PullIfNotPresent,
			}}}},
		},
	}
}

func createRequest(namespace, pool, requestID string) *fastpathv1.CreateRequest {
	return &fastpathv1.CreateRequest{
		Namespace: namespace, PoolRef: pool, RequestId: requestID,
		Image: "docker.io/library/alpine:latest", Command: []string{"/bin/sh", "-c", "sleep 3600"},
	}
}

func createNamespace(ctx context.Context, t *testing.T, k8sClient client.Client, namespace string) {
	t.Helper()
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("create namespace %s: %v", namespace, err)
	}
}

func currentLeader(ctx context.Context, t *testing.T, k8sClient client.Client) string {
	t.Helper()
	var identity string
	waitUntil(ctx, t, "Controller leader lease", func() (bool, error) {
		var lease coordinationv1.Lease
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: controlPlaneNamespace, Name: leaderLeaseName}, &lease); err != nil {
			return false, err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return false, nil
		}
		identity = *lease.Spec.HolderIdentity
		return true, nil
	})
	return identity
}

func waitForReadyFastlets(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, pool string, want int) {
	t.Helper()
	waitUntil(ctx, t, "ready Fastlet Pod", func() (bool, error) {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool}); err != nil {
			return false, err
		}
		ready := 0
		for index := range pods.Items {
			for _, condition := range pods.Items[index].Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready++
					break
				}
			}
		}
		return ready >= want, nil
	})
}

func waitForSandbox(ctx context.Context, t *testing.T, k8sClient client.Client, key types.NamespacedName, predicate func(*apiv1alpha1.Sandbox) bool) *apiv1alpha1.Sandbox {
	t.Helper()
	var result apiv1alpha1.Sandbox
	waitUntil(ctx, t, "Sandbox "+key.String(), func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &result); err != nil {
			return false, err
		}
		return predicate(&result), nil
	})
	return result.DeepCopy()
}

func waitForDeploymentReady(ctx context.Context, t *testing.T, k8sClient client.Client, name string, replicas int32) {
	t.Helper()
	waitUntil(ctx, t, "Deployment "+name, func() (bool, error) {
		var deployment appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: controlPlaneNamespace, Name: name}, &deployment); err != nil {
			return false, err
		}
		return deployment.Status.ReadyReplicas == replicas && deployment.Status.UpdatedReplicas == replicas, nil
	})
}

func waitUntil(ctx context.Context, t *testing.T, description string, predicate func() (bool, error)) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		ready, err := predicate()
		if ready {
			return
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %s: %v (last error: %v)", description, ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
