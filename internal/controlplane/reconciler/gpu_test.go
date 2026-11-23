package reconciler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/internal/runtimeenv"
)

func TestGPUUpgradeDrainsOldPodWhenSurgeCannotGetGPU(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gpus      int32
		selector  string
		affinity  string
		request   string
		otherPod  bool
		wantDrain bool
	}{
		{name: "CPU upgrade waits", request: "1"},
		{name: "old GPU makes room", gpus: 1, selector: "a", request: "1", wantDrain: true},
		{name: "replacement selects another node", gpus: 1, selector: "b", request: "1"},
		{name: "replacement has affinity for another node", gpus: 1, affinity: "b", request: "1"},
		{name: "old GPU alone is insufficient", gpus: 1, selector: "a", request: "2"},
		{name: "another namespace keeps one GPU", gpus: 1, selector: "a", request: "2", otherPod: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, _, pool := newDrainHarness(t, nil)
			pool.Spec.SandboxResources.GPUs = tc.gpus
			r.DrainTimeout = time.Second
			now := time.Now()
			r.Now = func() time.Time { return now }
			nodeGPUs := "1"
			if tc.otherPod {
				nodeGPUs = "2"
				require.NoError(t, c.Create(t.Context(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "other-workload", Namespace: "another-namespace"},
					Spec:       corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "workload", Resources: gpuRequests("1")}}},
				}))
			}
			require.NoError(t, c.Create(t.Context(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"gpu-node": "a"}},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
					Allocatable: corev1.ResourceList{
						"nvidia.com/gpu": resource.MustParse(nodeGPUs), corev1.ResourcePods: resource.MustParse("10"),
					},
				},
			}))
			oldPod := getFastletPod(t, c, "fastlet-a")
			oldPod.Spec.NodeName = "node-a"
			oldPod.Spec.Containers = []corev1.Container{{Name: "fastlet", Resources: gpuRequests("1")}}
			require.NoError(t, c.Update(t.Context(), oldPod))
			newPod := getFastletPod(t, c, "fastlet-b")
			newPod.Annotations = map[string]string{placement.AnnotationPodTemplateHash: "new"}
			newPod.Spec.Containers = []corev1.Container{{Name: "fastlet", Resources: gpuRequests(tc.request)}}
			if tc.selector != "" {
				newPod.Spec.NodeSelector = map[string]string{"gpu-node": tc.selector}
			}
			if tc.affinity != "" {
				newPod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "gpu-node", Operator: corev1.NodeSelectorOpIn, Values: []string{tc.affinity}}},
					}}},
				}}
			}
			require.NoError(t, c.Update(t.Context(), newPod))
			newPod.Status.Phase = corev1.PodPending
			newPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/2 nodes are available: 1 Insufficient nvidia.com/gpu, 1 node(s) didn't match Pod's node affinity/selector."}}
			require.NoError(t, c.Status().Update(t.Context(), newPod))

			_, _, err := r.reconcileDraining(t.Context(), pool, []corev1.Pod{*oldPod, *newPod}, nil, 1, "new", false, nil, nil)
			require.NoError(t, err)
			oldPod = getFastletPod(t, c, "fastlet-a")
			require.Equal(t, tc.wantDrain, placement.PodDrainRequested(oldPod))
			if !tc.wantDrain {
				return
			}
			require.Equal(t, placement.DrainReasonPlannedUpgrade, oldPod.Annotations[placement.AnnotationDrainReason])
			now = now.Add(2 * time.Second)
			active := []apiv1alpha2.Sandbox{assignedSandbox("sandbox-a", oldPod.Name, string(oldPod.UID))}
			_, _, err = r.reconcileDraining(t.Context(), pool, []corev1.Pod{*oldPod, *newPod}, active, 1, "new", false, nil, nil)
			require.NoError(t, err)
			oldPod = getFastletPod(t, c, "fastlet-a")
			_, _, err = r.reconcileDraining(t.Context(), pool, []corev1.Pod{*oldPod, *newPod}, nil, 1, "new", false, nil, nil)
			require.NoError(t, err)
			require.True(t, apierrors.IsNotFound(c.Get(t.Context(), client.ObjectKeyFromObject(oldPod), &corev1.Pod{})))
		})
	}
}

func gpuRequests(count string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse(count)}}
}

func TestGPUPoolPodUsesSlotBudgetAndRuntimeEnvironment(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	catalog := runtimecatalog.Builtin()
	r := &SandboxPoolReconciler{Scheme: scheme, Catalog: catalog}
	cfg := runtimeenv.DefaultConfig()
	node := cfg.Environments[runtimeenv.DefaultEnvironment]
	node.Kubelet.Root = "/custom/kubelet"
	cfg.Environments[runtimeenv.DefaultEnvironment] = node
	plan, err := runtimeenv.Resolve(catalog, cfg, apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)

	pool := resourceAwareScalingPool()
	pool.Spec.Runtime = apiv1alpha2.RuntimeContainer
	pool.Spec.SandboxResources.GPUs = 2
	pool.Spec.MaxSandboxesPerPod = 3
	pool.Spec.FastletTemplate.Spec.Containers = []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}}
	pod, err := r.constructPodWithRuntimePlan(pool, plan)
	require.NoError(t, err)
	fastlet := containerForName(t, pod, "fastlet")
	key := corev1.ResourceName("nvidia.com/gpu")
	for _, resources := range []corev1.ResourceList{fastlet.Resources.Requests, fastlet.Resources.Limits} {
		quantity := resources[key]
		require.Equal(t, int64(6), quantity.Value())
	}
	require.Equal(t, "6", envValue(fastlet.Env, "FAST_SANDBOX_GPU_EXPECTED_DEVICES"))
	require.Equal(t, "fastlet", envValue(fastlet.Env, "FAST_SANDBOX_GPU_CONTAINER_NAME"))
	require.True(t, hasHostPath(pod, "/custom/kubelet/pod-resources"))
	require.Equal(t, pool.Spec.SandboxResources.Hash(), pod.Annotations["fast-sandbox.io/resource-profile-hash"])

	pool.Spec.FastletTemplate.Spec.Containers[0].Resources.Limits = corev1.ResourceList{key: resource.MustParse("8")}
	_, err = r.constructPodWithRuntimePlan(pool, plan)
	require.ErrorContains(t, err, "nvidia.com/gpu")
	pool.Spec.Runtime = apiv1alpha2.RuntimeGVisor
	require.Error(t, pool.Spec.ValidateGPUContract())
}

func TestCPUOnlyPoolDoesNotAcquireGPUResources(t *testing.T) {
	pool := resourceAwareScalingPool()
	pool.Spec.SandboxResources.GPUs = 0
	pod := pool.Spec.FastletTemplate.Spec.DeepCopy()
	before := pod.DeepCopy()
	require.NoError(t, applyGPUResources(pool, runtimeenv.ResolvedRuntimePlan{}, pod))
	require.Equal(t, before, pod)
}

func TestGPUModelSelectsNodesAndRejectsSelectorConflict(t *testing.T) {
	pool := resourceAwareScalingPool()
	pool.Spec.Runtime = apiv1alpha2.RuntimeContainer
	pool.Spec.SandboxResources.GPUs = 1
	pool.Spec.SandboxResources.GPURequirements = &apiv1alpha2.GPUDeviceRequirements{Model: "Tesla-V100S-PCIE-32GB"}
	pool.Spec.FastletTemplate.Spec.Containers = []corev1.Container{{Name: "fastlet"}}
	plan := runtimeenv.ResolvedRuntimePlan{Kubelet: runtimeenv.KubeletEnvironment{Root: "/var/lib/kubelet"}}
	pod := pool.Spec.FastletTemplate.Spec.DeepCopy()
	require.NoError(t, applyGPUResources(pool, plan, pod))
	require.Equal(t, "Tesla-V100S-PCIE-32GB", pod.NodeSelector["nvidia.com/gpu.product"])
	pod.NodeSelector["nvidia.com/gpu.product"] = "Other-GPU"
	require.ErrorContains(t, applyGPUResources(pool, plan, pod), "GPU model node selection")
}

func TestGPUAdmissionRevisionWaitsForCompatibleReplacement(t *testing.T) {
	now := time.Now()
	pool := resourceAwareScalingPool()
	pool.Spec.SandboxResources.GPUs = 1
	pool.Status.FastletRevision = "old"
	old := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Annotations: map[string]string{
		placement.AnnotationPodTemplateHash:     "old",
		"fast-sandbox.io/runtime-plan-revision": "runtime",
		"fast-sandbox.io/resource-profile-hash": "resources",
		"fast-sandbox.io/infra-revision":        "infra",
		"fast-sandbox.io/settings-revision":     "settings",
	}}}
	next := old.DeepCopy()
	next.Name = "new"
	next.Annotations[placement.AnnotationPodTemplateHash] = "new"
	next.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}}
	pods := []corev1.Pod{old, *next}

	// Keep work on the old revision while the replacement has no fresh heartbeat.
	observed := map[string]placement.FastletInfo{}
	require.Equal(t, "old", admissionFastletRevision(pool, next, pods, observed, now))

	for _, key := range []string{"fast-sandbox.io/runtime-plan-revision", "fast-sandbox.io/resource-profile-hash",
		"fast-sandbox.io/infra-revision", "fast-sandbox.io/settings-revision"} {
		changed := next.DeepCopy()
		changed.Annotations[key] = "changed"
		require.Equal(t, "new", admissionFastletRevision(pool, changed, pods, observed, now), key)
	}
	observed[podIdentity(next)] = placement.FastletInfo{
		PodReady: true, RuntimeReady: true, InfraReady: true, LastHeartbeat: now,
	}
	require.Equal(t, "new", admissionFastletRevision(pool, next, pods, observed, now))
	pool.Spec.SandboxResources.GPUs = 0
	require.Equal(t, "new", admissionFastletRevision(pool, next, pods, nil, now))
}
