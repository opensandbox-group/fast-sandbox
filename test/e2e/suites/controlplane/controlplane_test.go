package controlplane

import (
	"context"
	"fmt"
	"sort"
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

	t.Run("Controller-only deployment reconciles direct CRD creation", func(t *testing.T) {
		restoreFastPath := scaleFastPath(ctx, t, k8sClient, 0)
		defer restoreFastPath()
		waitForFastPathPodCount(ctx, t, k8sClient, 0)

		sandbox := &apiv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "declarative", Namespace: namespace},
			Spec: apiv1alpha1.SandboxSpec{
				Image: "docker.io/library/alpine:latest", PoolRef: pool.Name,
				Command: []string{"/bin/sh", "-c", "sleep 3600"},
			},
		}
		if err := k8sClient.Create(ctx, sandbox); err != nil {
			t.Fatalf("create declarative Sandbox without FastPath: %v", err)
		}
		ready := waitForSandbox(ctx, t, k8sClient, types.NamespacedName{Namespace: namespace, Name: sandbox.Name}, func(item *apiv1alpha1.Sandbox) bool {
			return item.Status.Assignment != nil && item.Status.RuntimeState == apiv1alpha1.ObservedStateReady
		})
		if ready.UID == "" {
			t.Fatal("declarative Sandbox has no durable Kubernetes UID")
		}

		restoreFastPath()
		waitForDeploymentReady(ctx, t, k8sClient, "fast-sandbox-fastpath", 3)
	})

	t.Run("role all supports the single-process topology", func(t *testing.T) {
		enableAllInOneTopology(ctx, t, k8sClient)

		endpoint, portForward, err := e2eenv.StartControllerPortForward(ctx, controlPlaneNamespace)
		if err != nil {
			t.Fatalf("start all-in-one FastPath port-forward: %v", err)
		}
		defer func() {
			if err := portForward.Cleanup(); err != nil {
				t.Errorf("cleanup all-in-one FastPath port-forward: %v", err)
			}
		}()
		connection, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		if err != nil {
			t.Fatalf("dial all-in-one FastPath: %v", err)
		}
		defer connection.Close()
		allInOne := fastpathv1.NewFastPathServiceClient(connection)

		var response *fastpathv1.CreateResponse
		waitUntil(ctx, t, "all-in-one Registry heartbeat and Create", func() (bool, error) {
			requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
			defer requestCancel()
			created, createErr := allInOne.CreateSandbox(requestCtx, createRequest(namespace, pool.Name, namespace+"-all-in-one"))
			if createErr == nil {
				response = created
				return true, nil
			}
			switch status.Code(createErr) {
			case codes.ResourceExhausted, codes.Unavailable:
				return false, nil
			default:
				return false, createErr
			}
		})
		ready := waitForSandbox(ctx, t, k8sClient, types.NamespacedName{Namespace: namespace, Name: response.SandboxName}, func(item *apiv1alpha1.Sandbox) bool {
			return item.Status.RuntimeState == apiv1alpha1.ObservedStateReady
		})
		if response.SandboxUid != string(ready.UID) {
			t.Fatalf("all-in-one returned UID %q, want CRD UID %q", response.SandboxUid, ready.UID)
		}
	})

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
	replicas, closeReplicas := dialFastPathReplicas(ctx, t, k8sClient)
	defer closeReplicas()

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
		waitUntil(ctx, t, "Fastlet platform diagnostics", func() (bool, error) {
			diagnostics, diagnosticsErr := fastPath.GetSandboxDiagnostics(ctx, &fastpathv1.SandboxDiagnosticsRequest{
				SandboxName: first.SandboxName, Namespace: namespace, Limit: 10,
			})
			if diagnosticsErr != nil {
				return false, nil
			}
			if !diagnostics.FastletReachable || diagnostics.RuntimeInstanceId == "" || len(diagnostics.Events) == 0 {
				return false, nil
			}
			return true, nil
		})
		conflict := createRequest(namespace, pool.Name, requestID)
		conflict.Image = "docker.io/library/busybox:latest"
		if _, err := fastPath.CreateSandbox(ctx, conflict); status.Code(err) != codes.AlreadyExists {
			t.Fatalf("same request ID with different spec: code=%s err=%v", status.Code(err), err)
		}
	})

	t.Run("every FastPath replica independently serves Create", func(t *testing.T) {
		seenUIDs := make(map[string]string, len(replicas))
		seenNames := make(map[string]string, len(replicas))
		for index, replica := range replicas {
			requestCtx, requestCancel := context.WithTimeout(ctx, 30*time.Second)
			response, createErr := replica.client.CreateSandbox(requestCtx, createRequest(namespace, pool.Name, fmt.Sprintf("%s-replica-%d", namespace, index)))
			requestCancel()
			if createErr != nil {
				t.Fatalf("FastPath replica %s CreateSandbox: %v", replica.name, createErr)
			}
			assertUniqueCreateIdentity(t, seenUIDs, seenNames, replica.name, response)
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
		successfulResponses := make([]*fastpathv1.CreateResponse, 0, capacityPool.Spec.MaxSandboxesPerPod)
		failures := make([]error, 0, requests)
		for index := 0; index < requests; index++ {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				requestCtx, requestCancel := context.WithTimeout(ctx, 30*time.Second)
				defer requestCancel()
				response, createErr := replicas[index%len(replicas)].client.CreateSandbox(requestCtx, createRequest(namespace, capacityPool.Name, fmt.Sprintf("%s-capacity-%d", namespace, index)))
				lock.Lock()
				defer lock.Unlock()
				if createErr == nil {
					successes++
					successfulResponses = append(successfulResponses, response)
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
		seenUIDs := make(map[string]string, len(successfulResponses))
		seenNames := make(map[string]string, len(successfulResponses))
		for index, response := range successfulResponses {
			assertUniqueCreateIdentity(t, seenUIDs, seenNames, fmt.Sprintf("response-%d", index), response)
		}
		waitUntil(ctx, t, "capacity-bounded ready runtimes and durable CRD-first intents", func() (bool, error) {
			var list apiv1alpha1.SandboxList
			if err := k8sClient.List(ctx, &list, client.InNamespace(namespace)); err != nil {
				return false, err
			}
			readyCount := 0
			intentCount := 0
			for index := range list.Items {
				if list.Items[index].Spec.PoolRef == capacityPool.Name {
					intentCount++
					if list.Items[index].Annotations["sandbox.fast.io/assignment"] == "" {
						return false, fmt.Errorf("CRD-first Sandbox %s/%s has no durable assignment annotation", list.Items[index].Namespace, list.Items[index].Name)
					}
					if list.Items[index].Status.RuntimeState == apiv1alpha1.ObservedStateReady {
						if owner, exists := seenUIDs[string(list.Items[index].UID)]; !exists || owner == "" {
							return false, fmt.Errorf("ready Sandbox %s/%s has unreported RPC identity %q", list.Items[index].Namespace, list.Items[index].Name, list.Items[index].UID)
						}
						readyCount++
					}
				}
			}
			if readyCount > successes {
				return false, fmt.Errorf("ready runtimes=%d exceeds successful admissions=%d", readyCount, successes)
			}
			return readyCount == successes && intentCount >= successes && intentCount <= requests, nil
		})
	})
}

type fastPathReplica struct {
	name       string
	client     fastpathv1.FastPathServiceClient
	connection *grpc.ClientConn
	forward    interface{ Cleanup() error }
}

func dialFastPathReplicas(ctx context.Context, t *testing.T, k8sClient client.Client) ([]fastPathReplica, func()) {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(controlPlaneNamespace), client.MatchingLabels{"app": "fast-sandbox-fastpath"}); err != nil {
		t.Fatalf("list FastPath replicas: %v", err)
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	if len(pods.Items) != 3 {
		t.Fatalf("FastPath replica count=%d, want 3", len(pods.Items))
	}
	replicas := make([]fastPathReplica, 0, len(pods.Items))
	cleanup := func() {
		for index := len(replicas) - 1; index >= 0; index-- {
			if err := replicas[index].connection.Close(); err != nil {
				t.Errorf("close FastPath replica %s connection: %v", replicas[index].name, err)
			}
			if err := replicas[index].forward.Cleanup(); err != nil {
				t.Errorf("cleanup FastPath replica %s port-forward: %v", replicas[index].name, err)
			}
		}
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !podReady(pod) {
			cleanup()
			t.Fatalf("FastPath replica %s is not Ready", pod.Name)
		}
		endpoint, forward, err := e2eenv.StartPodTCPPortForward(ctx, pod.Namespace, pod.Name, 9090)
		if err != nil {
			cleanup()
			t.Fatalf("start FastPath replica %s port-forward: %v", pod.Name, err)
		}
		dialCtx, dialCancel := context.WithTimeout(ctx, 20*time.Second)
		connection, err := grpc.DialContext(dialCtx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		dialCancel()
		if err != nil {
			_ = forward.Cleanup()
			cleanup()
			t.Fatalf("dial FastPath replica %s: %v", pod.Name, err)
		}
		replicas = append(replicas, fastPathReplica{
			name: pod.Name, client: fastpathv1.NewFastPathServiceClient(connection), connection: connection, forward: forward,
		})
	}
	return replicas, cleanup
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func assertUniqueCreateIdentity(t *testing.T, seenUIDs, seenNames map[string]string, owner string, response *fastpathv1.CreateResponse) {
	t.Helper()
	if response == nil || response.SandboxUid == "" || response.SandboxName == "" {
		t.Fatalf("%s returned incomplete Create identity: %+v", owner, response)
	}
	if previous, exists := seenUIDs[response.SandboxUid]; exists {
		t.Fatalf("duplicate Sandbox UID %q returned by %s and %s", response.SandboxUid, previous, owner)
	}
	if previous, exists := seenNames[response.SandboxName]; exists {
		t.Fatalf("duplicate Sandbox name %q returned by %s and %s", response.SandboxName, previous, owner)
	}
	seenUIDs[response.SandboxUid] = owner
	seenNames[response.SandboxName] = owner
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

func scaleFastPath(ctx context.Context, t *testing.T, k8sClient client.Client, replicas int32) func() {
	t.Helper()
	key := types.NamespacedName{Namespace: controlPlaneNamespace, Name: "fast-sandbox-fastpath"}
	var deployment appsv1.Deployment
	if err := k8sClient.Get(ctx, key, &deployment); err != nil {
		t.Fatalf("get FastPath Deployment before scale: %v", err)
	}
	original := int32(1)
	if deployment.Spec.Replicas != nil {
		original = *deployment.Spec.Replicas
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var current appsv1.Deployment
		if err := k8sClient.Get(restoreCtx, key, &current); err != nil {
			t.Errorf("get FastPath Deployment for restore: %v", err)
			return
		}
		current.Spec.Replicas = &original
		if err := k8sClient.Update(restoreCtx, &current); err != nil {
			t.Errorf("restore FastPath replicas to %d: %v", original, err)
			return
		}
		restored = true
	}
	deployment.Spec.Replicas = &replicas
	if err := k8sClient.Update(ctx, &deployment); err != nil {
		t.Fatalf("scale FastPath to %d: %v", replicas, err)
	}
	return restore
}

func waitForFastPathPodCount(ctx context.Context, t *testing.T, k8sClient client.Client, want int) {
	t.Helper()
	waitUntil(ctx, t, fmt.Sprintf("FastPath Pod count %d", want), func() (bool, error) {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(controlPlaneNamespace), client.MatchingLabels{"app": "fast-sandbox-fastpath"}); err != nil {
			return false, err
		}
		return len(pods.Items) == want, nil
	})
}

func enableAllInOneTopology(ctx context.Context, t *testing.T, k8sClient client.Client) {
	t.Helper()
	controllerKey := types.NamespacedName{Namespace: controlPlaneNamespace, Name: "fast-sandbox-controller"}
	fastPathKey := types.NamespacedName{Namespace: controlPlaneNamespace, Name: "fast-sandbox-fastpath"}

	var originalController appsv1.Deployment
	if err := k8sClient.Get(ctx, controllerKey, &originalController); err != nil {
		t.Fatalf("get Controller Deployment before all-in-one transition: %v", err)
	}
	var originalFastPath appsv1.Deployment
	if err := k8sClient.Get(ctx, fastPathKey, &originalFastPath); err != nil {
		t.Fatalf("get FastPath Deployment before all-in-one transition: %v", err)
	}
	var originalService corev1.Service
	if err := k8sClient.Get(ctx, fastPathKey, &originalService); err != nil {
		t.Fatalf("get FastPath Service before all-in-one transition: %v", err)
	}
	var originalHPA autoscalingv2.HorizontalPodAutoscaler
	if err := k8sClient.Get(ctx, fastPathKey, &originalHPA); err != nil {
		t.Fatalf("get FastPath HPA before all-in-one transition: %v", err)
	}
	hpaDeleted := false

	t.Cleanup(func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer restoreCancel()

		var currentFastPath appsv1.Deployment
		if err := k8sClient.Get(restoreCtx, fastPathKey, &currentFastPath); err != nil {
			t.Errorf("get FastPath Deployment for all-in-one restore: %v", err)
		} else {
			currentFastPath.Spec = originalFastPath.Spec
			currentFastPath.Labels = originalFastPath.Labels
			if err := k8sClient.Update(restoreCtx, &currentFastPath); err != nil {
				t.Errorf("restore FastPath Deployment after all-in-one test: %v", err)
			} else {
				waitForDeploymentReady(restoreCtx, t, k8sClient, originalFastPath.Name, *originalFastPath.Spec.Replicas)
			}
		}
		if hpaDeleted {
			restoredHPA := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: originalHPA.Name, Namespace: originalHPA.Namespace,
					Labels: originalHPA.Labels, Annotations: originalHPA.Annotations,
				},
				Spec: originalHPA.Spec,
			}
			if err := k8sClient.Create(restoreCtx, restoredHPA); err != nil {
				t.Errorf("restore FastPath HPA after all-in-one test: %v", err)
			}
		}

		var currentService corev1.Service
		if err := k8sClient.Get(restoreCtx, fastPathKey, &currentService); err != nil {
			t.Errorf("get FastPath Service for all-in-one restore: %v", err)
		} else {
			currentService.Spec.Selector = originalService.Spec.Selector
			if err := k8sClient.Update(restoreCtx, &currentService); err != nil {
				t.Errorf("restore FastPath Service after all-in-one test: %v", err)
			}
		}

		var currentController appsv1.Deployment
		if err := k8sClient.Get(restoreCtx, controllerKey, &currentController); err != nil {
			t.Errorf("get Controller Deployment for all-in-one restore: %v", err)
		} else {
			currentController.Spec = originalController.Spec
			currentController.Labels = originalController.Labels
			if err := k8sClient.Update(restoreCtx, &currentController); err != nil {
				t.Errorf("restore Controller Deployment after all-in-one test: %v", err)
			} else {
				waitForDeploymentReady(restoreCtx, t, k8sClient, originalController.Name, *originalController.Spec.Replicas)
			}
		}
	})

	controller := originalController.DeepCopy()
	one := int32(1)
	controller.Spec.Replicas = &one
	controller.Labels["fast-sandbox.io/control-plane-role"] = "all"
	controller.Spec.Template.Labels["fast-sandbox.io/control-plane-role"] = "all"
	manager := &controller.Spec.Template.Spec.Containers[0]
	manager.Args = replaceArgument(manager.Args, "--role=", "--role=all")
	if !hasArgumentPrefix(manager.Args, "--fastpath-bind-address=") {
		manager.Args = append(manager.Args, "--fastpath-bind-address=:9090")
	}
	if !hasEnv(manager.Env, "FAST_SANDBOX_ROUTE_SIGNING_PRIVATE_KEY") {
		manager.Env = append(manager.Env, corev1.EnvVar{
			Name: "FAST_SANDBOX_ROUTE_SIGNING_PRIVATE_KEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "fast-sandbox-route-keys"}, Key: "private-key",
			}},
		})
	}
	if !hasContainerPort(manager.Ports, "grpc") {
		manager.Ports = append(manager.Ports, corev1.ContainerPort{Name: "grpc", ContainerPort: 9090})
	}
	if err := k8sClient.Update(ctx, controller); err != nil {
		t.Fatalf("transition Controller Deployment to role=all: %v", err)
	}
	waitForDeploymentReady(ctx, t, k8sClient, controller.Name, 1)

	service := originalService.DeepCopy()
	service.Spec.Selector = map[string]string{
		"app": "fast-sandbox-controller", "fast-sandbox.io/control-plane-role": "all",
	}
	if err := k8sClient.Update(ctx, service); err != nil {
		t.Fatalf("route FastPath Service to role=all Pod: %v", err)
	}

	if err := k8sClient.Delete(ctx, &originalHPA); err != nil {
		t.Fatalf("temporarily remove FastPath HPA for all-in-one test: %v", err)
	}
	hpaDeleted = true
	fastPath := originalFastPath.DeepCopy()
	zero := int32(0)
	fastPath.Spec.Replicas = &zero
	if err := k8sClient.Update(ctx, fastPath); err != nil {
		t.Fatalf("scale separate FastPath Deployment to zero for all-in-one test: %v", err)
	}
	waitForFastPathPodCount(ctx, t, k8sClient, 0)
	waitUntil(ctx, t, "FastPath Service selecting one role=all Pod", func() (bool, error) {
		var endpoints corev1.Endpoints
		if err := k8sClient.Get(ctx, fastPathKey, &endpoints); err != nil {
			return false, err
		}
		addresses := make([]corev1.EndpointAddress, 0, 1)
		for _, subset := range endpoints.Subsets {
			addresses = append(addresses, subset.Addresses...)
		}
		return len(addresses) == 1 && addresses[0].TargetRef != nil &&
			strings.HasPrefix(addresses[0].TargetRef.Name, "fast-sandbox-controller-"), nil
	})
}

func replaceArgument(arguments []string, prefix, value string) []string {
	result := append([]string(nil), arguments...)
	for index := range result {
		if strings.HasPrefix(result[index], prefix) {
			result[index] = value
			return result
		}
	}
	return append(result, value)
}

func hasArgumentPrefix(arguments []string, prefix string) bool {
	for _, argument := range arguments {
		if strings.HasPrefix(argument, prefix) {
			return true
		}
	}
	return false
}

func hasEnv(environment []corev1.EnvVar, name string) bool {
	for _, variable := range environment {
		if variable.Name == name {
			return true
		}
	}
	return false
}

func hasContainerPort(ports []corev1.ContainerPort, name string) bool {
	for _, port := range ports {
		if port.Name == name {
			return true
		}
	}
	return false
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
