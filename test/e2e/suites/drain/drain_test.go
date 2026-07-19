package drain

import (
	"context"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
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
			if first.Status.Assignment == nil || second.Status.Assignment == nil || first.Status.Assignment.FastletPodUID == second.Status.Assignment.FastletPodUID {
				t.Fatalf("expected loaded Sandboxes on distinct Fastlets, got first=%+v second=%+v", first.Status.Assignment, second.Status.Assignment)
			}

			requireNoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var current apiv1alpha1.SandboxPool
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
					if fastletpool.PodDrainRequested(&pods.Items[index]) {
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
			if drainingPod.Annotations[fastletpool.AnnotationDrainStartedAt] == "" || drainingPod.Annotations[fastletpool.AnnotationDrainAckedAt] == "" {
				t.Fatalf("drain intent was not durably started and acknowledged: %#v", drainingPod.Annotations)
			}
			restartControllerLeader(ctx, t, k8sClient)
			var persisted corev1.Pod
			requireNoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: drainingPod.Name}, &persisted), "get draining Pod after leader change")
			if persisted.UID != drainingPod.UID || !fastletpool.PodDrainRequested(&persisted) {
				t.Fatalf("leader change lost durable drain state: before=%s/%s after=%s/%s", drainingPod.Name, drainingPod.UID, persisted.Name, persisted.UID)
			}

			third := drainSandbox(namespace, "sandbox-c", pool.Name, "docker.io/library/alpine:latest")
			requireNoError(t, k8sClient.Create(ctx, third), "create post-drain Sandbox")
			third = waitAssigned(ctx, t, fixture, third)
			if third.Status.Assignment.FastletPodUID == string(drainingPod.UID) {
				t.Fatalf("new Sandbox was assigned to draining Pod %s/%s", drainingPod.Name, drainingPod.UID)
			}
			return ctx
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func drainPool(namespace string) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		TypeMeta: metav1.TypeMeta{APIVersion: apiv1alpha1.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "drain-pool", Namespace: namespace},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity: apiv1alpha1.PoolCapacity{PoolMin: 2, PoolMax: 2}, MaxSandboxesPerPod: 5,
			Runtime: apiv1alpha1.RuntimeContainer, SandboxResources: suiteenv.SmallSandboxResourceProfile(),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: suiteenv.FastletImage()}}}},
		},
	}
}

func drainSandbox(namespace, name, pool, image string) *apiv1alpha1.Sandbox {
	return &apiv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: apiv1alpha1.GroupVersion.String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: apiv1alpha1.SandboxSpec{Image: image, Command: []string{"/bin/sleep", "3600"}, PoolRef: pool},
	}
}

func waitAssigned(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, sandbox *apiv1alpha1.Sandbox) *apiv1alpha1.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	result, err := fixture.WaitForSandbox(waitCtx, client.ObjectKeyFromObject(sandbox), func(current *apiv1alpha1.Sandbox) bool {
		return current.Status.Assignment != nil && current.Status.RuntimeState == apiv1alpha1.ObservedStateReady
	})
	requireNoError(t, err, "wait for Sandbox assignment")
	return result
}

func requireNoError(t *testing.T, err error, action string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

func restartControllerLeader(ctx context.Context, t *testing.T, k8sClient client.Client) {
	t.Helper()
	const leaseName = "fast-sandbox-controller.sandbox.fast.io"
	var before string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var lease coordinationv1.Lease
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: leaseName}, &lease); err == nil && lease.Spec.HolderIdentity != nil {
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
	requireNoError(t, k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: leaderPod, Namespace: "default"}}, &client.DeleteOptions{GracePeriodSeconds: &zero}), "delete Controller leader")
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var lease coordinationv1.Lease
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: leaseName}, &lease); err == nil &&
			lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" && *lease.Spec.HolderIdentity != before {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("Controller leader did not change before timeout")
}
