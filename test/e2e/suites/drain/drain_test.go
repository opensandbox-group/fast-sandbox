package drain

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"
)

func TestPoolScaleDownUsesDurableDrain(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("durable-pool-drain").
		WithLabel("suite", "drain").
		WithLabel("tier", "smoke").
		Assess("loaded Fastlet is fenced before scale-down deletion", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("drain")
			requireNoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}), "create namespace")
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := drainPool(namespace)
			_, err := fixture.CreateSandboxPool(ctx, namespace, pool)
			requireNoError(t, err, "create Pool")
			waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			_, err = fixture.WaitForReadyFastletPods(waitCtx, types.NamespacedName{Namespace: namespace, Name: pool.Name}, 2)
			requireNoError(t, err, "wait for two Fastlets")

			first := drainSandbox(namespace, "sandbox-a", pool.Name, "docker.io/library/alpine:latest")
			requireNoError(t, k8sClient.Create(ctx, first), "create first Sandbox")
			first = waitAssigned(ctx, t, fixture, first)
			// Let the low-frequency heartbeat publish the first allocation so the
			// second request is deliberately spread to the idle Fastlet.
			time.Sleep(25 * time.Second)
			second := drainSandbox(namespace, "sandbox-b", pool.Name, "docker.io/library/alpine:latest")
			requireNoError(t, k8sClient.Create(ctx, second), "create second Sandbox")
			second = waitAssigned(ctx, t, fixture, second)
			if first.Status.Placement.FastletName == "" || second.Status.Placement.FastletName == "" || first.Status.Placement.FastletPodUID == second.Status.Placement.FastletPodUID {
				t.Fatalf("expected loaded Sandboxes on distinct Fastlets, got first=%+v second=%+v", first.Status.Placement, second.Status.Placement)
			}

			requireNoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var current apiv1alpha2.SandboxPool
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &current); err != nil {
					return err
				}
				current.Spec.Capacity.PoolMin = 1
				return k8sClient.Update(ctx, &current)
			}), "reduce Pool minimum")

			var drainingPod corev1.Pod
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				var pods corev1.PodList
				requireNoError(t, k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool.Name}), "list Fastlets")
				for index := range pods.Items {
					if placement.PodDrainRequested(&pods.Items[index]) {
						drainingPod = pods.Items[index]
						break
					}
				}
				if drainingPod.Name != "" {
					if len(pods.Items) != 2 {
						t.Fatalf("loaded Fastlet was deleted instead of being drained; Pods=%d", len(pods.Items))
					}
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			if drainingPod.Name == "" {
				t.Fatal("no Fastlet received the durable drain annotation")
			}
			if drainingPod.Annotations[placement.AnnotationDrainStartedAt] == "" || drainingPod.Annotations[placement.AnnotationDrainAckedAt] == "" {
				t.Fatalf("drain intent was not durably started and acknowledged: %#v", drainingPod.Annotations)
			}
			restartControllerLeader(ctx, t, k8sClient)
			var persisted corev1.Pod
			requireNoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: drainingPod.Name}, &persisted), "get draining Pod after leader change")
			if persisted.UID != drainingPod.UID || !placement.PodDrainRequested(&persisted) {
				t.Fatalf("leader change lost durable drain state: before=%s/%s after=%s/%s", drainingPod.Name, drainingPod.UID, persisted.Name, persisted.UID)
			}

			third := drainSandbox(namespace, "sandbox-c", pool.Name, "docker.io/library/alpine:latest")
			requireNoError(t, k8sClient.Create(ctx, third), "create post-drain Sandbox")
			third = waitAssigned(ctx, t, fixture, third)
			if third.Status.Placement.FastletPodUID == drainingPod.UID {
				t.Fatalf("new Sandbox was assigned to draining Pod %s/%s", drainingPod.Name, drainingPod.UID)
			}
			return ctx
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func TestResourceAwareScaleDownUsesDurableDrain(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("resource-aware-durable-pool-drain").
		WithLabel("suite", "drain").
		WithLabel("tier", "validation").
		Assess("only an empty ResourceAware Fastlet is durably fenced before scale-down", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("resource-drain")
			requireNoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}), "create namespace")
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := resourceAwareDrainPool(namespace)
			_, err := fixture.CreateSandboxPool(ctx, namespace, pool)
			requireNoError(t, err, "create ResourceAware Pool")
			waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			_, err = fixture.WaitForReadyFastletPods(waitCtx, client.ObjectKeyFromObject(pool), 2)
			requireNoError(t, err, "wait for two ResourceAware Fastlets")

			first := drainSandbox(namespace, "resource-sandbox-a", pool.Name, "docker.io/library/alpine:latest")
			requireNoError(t, k8sClient.Create(ctx, first), "create first ResourceAware Sandbox")
			first = waitAssigned(ctx, t, fixture, first)
			second := drainSandbox(namespace, "resource-sandbox-b", pool.Name, "docker.io/library/alpine:latest")
			requireNoError(t, k8sClient.Create(ctx, second), "create second ResourceAware Sandbox")
			second = waitAssigned(ctx, t, fixture, second)
			if first.Status.Placement.FastletPodUID == second.Status.Placement.FastletPodUID {
				t.Fatalf("ResourceAware Sandboxes were not spread across Fastlets: first=%+v second=%+v", first.Status.Placement, second.Status.Placement)
			}

			const holdFinalizer = "e2e.fast-sandbox.io/hold-draining-pod"
			var pods corev1.PodList
			requireNoError(t, k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool.Name}), "list ResourceAware Fastlets")
			for index := range pods.Items {
				pod := &pods.Items[index]
				requireNoError(t, addPodFinalizer(ctx, k8sClient, client.ObjectKeyFromObject(pod), holdFinalizer), "hold ResourceAware Fastlet deletion")
			}
			requireNoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var current apiv1alpha2.SandboxPool
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &current); err != nil {
					return err
				}
				current.Spec.Capacity.PoolMin = 1
				return k8sClient.Update(ctx, &current)
			}), "reduce ResourceAware Pool minimum")
			if err := ensureNoDrainingPod(ctx, k8sClient, namespace, pool.Name, 5*time.Second); err != nil {
				t.Fatalf("occupied ResourceAware Fastlets must not be selected for scale-down: %v", err)
			}

			// Once one Sandbox is deleted, exactly that empty Fastlet becomes a
			// valid scale-down candidate after the stabilization window.
			requireNoError(t, k8sClient.Delete(ctx, first), "delete first ResourceAware Sandbox")
			deleteCtx, cancelDelete := context.WithTimeout(ctx, 60*time.Second)
			requireNoError(t, fixture.WaitForSandboxDeleted(deleteCtx, client.ObjectKeyFromObject(first)), "wait for first ResourceAware Sandbox deletion")
			cancelDelete()

			drainingPod := waitForDrainingPod(ctx, t, k8sClient, namespace, pool.Name, 100*time.Second)
			if drainingPod.UID != first.Status.Placement.FastletPodUID {
				t.Fatalf("draining Pod UID=%s, want newly empty Fastlet UID=%s", drainingPod.UID, first.Status.Placement.FastletPodUID)
			}
			if drainingPod.Annotations[placement.AnnotationDrainReason] != placement.DrainReasonScaleDown ||
				drainingPod.Annotations[placement.AnnotationDrainStartedAt] == "" || drainingPod.Annotations[placement.AnnotationDrainAckedAt] == "" {
				t.Fatalf("ResourceAware drain metadata is incomplete: %#v", drainingPod.Annotations)
			}

			restartControllerLeader(ctx, t, k8sClient)
			var persisted corev1.Pod
			requireNoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(&drainingPod), &persisted), "get ResourceAware draining Pod after leader change")
			if persisted.UID != drainingPod.UID || !placement.PodDrainRequested(&persisted) {
				t.Fatalf("leader change lost ResourceAware drain state: before=%s/%s after=%s/%s", drainingPod.Name, drainingPod.UID, persisted.Name, persisted.UID)
			}

			third := drainSandbox(namespace, "resource-sandbox-c", pool.Name, "docker.io/library/alpine:latest")
			requireNoError(t, k8sClient.Create(ctx, third), "create post-drain ResourceAware Sandbox")
			third = waitAssigned(ctx, t, fixture, third)
			if third.Status.Placement.FastletPodUID == drainingPod.UID {
				t.Fatalf("new ResourceAware Sandbox was assigned to draining Pod %s/%s", drainingPod.Name, drainingPod.UID)
			}

			requireNoError(t, removePodFinalizer(ctx, k8sClient, client.ObjectKeyFromObject(&drainingPod), holdFinalizer), "release draining Fastlet deletion")
			deadline := time.Now().Add(60 * time.Second)
			for time.Now().Before(deadline) {
				var current corev1.Pod
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&drainingPod), &current)
				if apierrors.IsNotFound(err) {
					return ctx
				}
				requireNoError(t, err, "wait for ResourceAware draining Fastlet deletion")
				time.Sleep(250 * time.Millisecond)
			}
			t.Fatal("ResourceAware draining Fastlet was not deleted after its hold finalizer was removed")
			return ctx
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func TestPoolPlannedUpgradeUsesReadySurgeAndDurableDrain(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("planned-upgrade-drain").
		WithLabel("suite", "drain").
		WithLabel("tier", "smoke").
		Assess("replacement is Ready before an outdated loaded Fastlet is drained", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("upgrade")
			requireNoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}), "create namespace")
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := drainPool(namespace)
			pool.Name = "upgrade-pool"
			pool.Spec.Capacity = apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1}
			_, err := fixture.CreateSandboxPool(ctx, namespace, pool)
			requireNoError(t, err, "create upgrade Pool")
			waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			_, err = fixture.WaitForReadyFastletPods(waitCtx, client.ObjectKeyFromObject(pool), 1)
			requireNoError(t, err, "wait for initial Fastlet")
			var initialPods corev1.PodList
			requireNoError(t, k8sClient.List(ctx, &initialPods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool.Name}), "list initial Fastlet")
			if len(initialPods.Items) != 1 {
				t.Fatalf("initial Fastlet count=%d, want 1", len(initialPods.Items))
			}
			oldPod := initialPods.Items[0]
			oldHash := oldPod.Annotations[placement.AnnotationPodTemplateHash]
			if oldHash == "" {
				t.Fatal("initial Fastlet has no desired Pod template hash")
			}

			sandbox := drainSandbox(namespace, "upgrade-sandbox", pool.Name, "docker.io/library/alpine:latest")
			requireNoError(t, k8sClient.Create(ctx, sandbox), "create loaded Sandbox")
			sandbox = waitAssigned(ctx, t, fixture, sandbox)
			if sandbox.Status.Placement.FastletPodUID != oldPod.UID {
				t.Fatalf("Sandbox assigned to %s, want initial Fastlet %s", sandbox.Status.Placement.FastletPodUID, oldPod.UID)
			}

			requireNoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var current apiv1alpha2.SandboxPool
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &current); err != nil {
					return err
				}
				current.Spec.WarmImages = append(current.Spec.WarmImages, "docker.io/library/busybox:latest")
				return k8sClient.Update(ctx, &current)
			}), "change desired Fastlet Pod configuration")

			_, err = fixture.WaitForReadyFastletPods(waitCtx, client.ObjectKeyFromObject(pool), 2)
			requireNoError(t, err, "wait for Ready surge Fastlet")
			var drainingOld corev1.Pod
			var replacement corev1.Pod
			deadline := time.Now().Add(90 * time.Second)
			for time.Now().Before(deadline) {
				var pods corev1.PodList
				requireNoError(t, k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool.Name}), "list upgrade Fastlets")
				for index := range pods.Items {
					pod := pods.Items[index]
					if pod.UID == oldPod.UID {
						if placement.PodDrainRequested(&pod) {
							drainingOld = pod
						}
						continue
					}
					if pod.Annotations[placement.AnnotationPodTemplateHash] != oldHash && podReady(&pod) {
						replacement = pod
					}
				}
				if drainingOld.Name != "" && replacement.Name != "" {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			if replacement.Name == "" {
				t.Fatal("no Ready replacement Fastlet with the new template hash")
			}
			if drainingOld.Name == "" {
				t.Fatal("outdated loaded Fastlet was not durably drained after replacement became Ready")
			}
			if drainingOld.Annotations[placement.AnnotationDrainReason] != placement.DrainReasonPlannedUpgrade || drainingOld.Annotations[placement.AnnotationDrainAckedAt] == "" {
				t.Fatalf("planned upgrade drain metadata is incomplete: %#v", drainingOld.Annotations)
			}

			requireNoError(t, k8sClient.Delete(ctx, sandbox), "delete loaded Sandbox")
			deadline = time.Now().Add(90 * time.Second)
			for time.Now().Before(deadline) {
				var current corev1.Pod
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: oldPod.Name}, &current)
				if apierrors.IsNotFound(err) {
					return ctx
				}
				requireNoError(t, err, "wait for drained old Fastlet deletion")
				time.Sleep(250 * time.Millisecond)
			}
			t.Fatal("old Fastlet was not deleted after its Sandbox completed declarative deletion")
			return ctx
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func drainPool(namespace string) *apiv1alpha2.SandboxPool {
	return &apiv1alpha2.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha2.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "drain-pool", Namespace: namespace},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Capacity: apiv1alpha2.PoolCapacity{PoolMin: 2, PoolMax: 2}, MaxSandboxesPerPod: 5,
			Runtime: apiv1alpha2.RuntimeContainer, SandboxResources: suiteenv.SmallSandboxResourceProfile(),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: suiteenv.FastletImage()}}}},
		},
	}
}

func resourceAwareDrainPool(namespace string) *apiv1alpha2.SandboxPool {
	pool := drainPool(namespace)
	pool.Name = "resource-drain-pool"
	pool.Spec.MaxSandboxesPerPod = 2
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

func drainSandbox(namespace, name, pool, image string) *apiv1alpha2.Sandbox {
	return &apiv1alpha2.Sandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha2.GroupVersion.String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       apiv1alpha2.SandboxSpec{Image: image, Command: []string{"/bin/sleep", "3600"}, PoolRef: pool},
	}
}

func waitAssigned(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, sandbox *apiv1alpha2.Sandbox) *apiv1alpha2.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	result, err := fixture.WaitForSandbox(waitCtx, client.ObjectKeyFromObject(sandbox), func(current *apiv1alpha2.Sandbox) bool {
		return current.Status.Placement.FastletName != "" && current.Status.Runtime.State == apiv1alpha2.RuntimeReady
	})
	requireNoError(t, err, "wait for Sandbox assignment")
	return result
}

func podReady(pod *corev1.Pod) bool {
	if pod == nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func requireNoError(t *testing.T, err error, action string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

func addPodFinalizer(ctx context.Context, k8sClient client.Client, key client.ObjectKey, finalizer string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var pod corev1.Pod
		if err := k8sClient.Get(ctx, key, &pod); err != nil {
			return err
		}
		for _, existing := range pod.Finalizers {
			if existing == finalizer {
				return nil
			}
		}
		pod.Finalizers = append(pod.Finalizers, finalizer)
		return k8sClient.Update(ctx, &pod)
	})
}

func removePodFinalizer(ctx context.Context, k8sClient client.Client, key client.ObjectKey, finalizer string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var pod corev1.Pod
		if err := k8sClient.Get(ctx, key, &pod); err != nil {
			return client.IgnoreNotFound(err)
		}
		filtered := pod.Finalizers[:0]
		for _, existing := range pod.Finalizers {
			if existing != finalizer {
				filtered = append(filtered, existing)
			}
		}
		pod.Finalizers = filtered
		return k8sClient.Update(ctx, &pod)
	})
}

func ensureNoDrainingPod(ctx context.Context, k8sClient client.Client, namespace, pool string, duration time.Duration) error {
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
		var pods corev1.PodList
		if err := k8sClient.List(checkCtx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool}); err != nil {
			if checkCtx.Err() == context.DeadlineExceeded {
				return nil
			}
			return err
		}
		for index := range pods.Items {
			if placement.PodDrainRequested(&pods.Items[index]) {
				return fmt.Errorf("occupied Pod %s was marked draining", pods.Items[index].Name)
			}
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

func waitForDrainingPod(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, pool string, timeout time.Duration) corev1.Pod {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pods corev1.PodList
		if err := k8sClient.List(waitCtx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool}); err == nil {
			for index := range pods.Items {
				if placement.PodDrainRequested(&pods.Items[index]) && pods.Items[index].Annotations[placement.AnnotationDrainAckedAt] != "" {
					return pods.Items[index]
				}
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for durable ResourceAware drain: %v", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func restartControllerLeader(ctx context.Context, t *testing.T, k8sClient client.Client) {
	t.Helper()
	const leaseName = "fast-sandbox-reconciler.sandbox.fast.io"
	controlPlaneNamespace := testSuite.ControllerNamespace()
	var before string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var lease coordinationv1.Lease
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: controlPlaneNamespace, Name: leaseName}, &lease); err == nil && lease.Spec.HolderIdentity != nil {
			before = *lease.Spec.HolderIdentity
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	leaderPod := strings.SplitN(before, "_", 2)[0]
	if leaderPod == "" {
		t.Fatalf("invalid Controller leader identity %q", before)
	}
	zero := int64(0)
	requireNoError(t, k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: leaderPod, Namespace: controlPlaneNamespace}}, &client.DeleteOptions{GracePeriodSeconds: &zero}), "delete Controller leader")
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var lease coordinationv1.Lease
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: controlPlaneNamespace, Name: leaseName}, &lease); err == nil &&
			lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" && *lease.Spec.HolderIdentity != before {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("Controller leader did not change before timeout")
}
