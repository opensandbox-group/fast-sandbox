package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type legacyHeartbeatDrainer struct {
	setDraining func(*fastletapi.SetDrainingRequest) (*fastletapi.SetDrainingResponse, error)
	heartbeat   func() (*fastletapi.HeartbeatResponse, error)
}

func (d *legacyHeartbeatDrainer) SetDraining(_ context.Context, _ string, request *fastletapi.SetDrainingRequest) (*fastletapi.SetDrainingResponse, error) {
	return d.setDraining(request)
}

func (d *legacyHeartbeatDrainer) Heartbeat(context.Context, string, *fastletapi.HeartbeatRequest) (*fastletapi.HeartbeatResponse, error) {
	return d.heartbeat()
}

func emptyLegacyDrainHeartbeat(pod *corev1.Pod) *fastletapi.HeartbeatResponse {
	return &fastletapi.HeartbeatResponse{FastletStatus: fastletapi.FastletStatus{
		FastletPodUID: string(pod.UID), Draining: true, RuntimeReady: true,
		Admission: fastletapi.AdmissionStatus{Resources: &fastletapi.ResourceAdmissionStatus{
			Version: "v2", Epoch: "epoch-a", Revision: 1,
			Committed: fastletapi.SandboxResourceVector{CPU: "0", Memory: "0"},
		}},
	}}
}

func TestLegacyDrainRequiresFreshValidHeartbeat(t *testing.T) {
	for _, test := range []struct {
		name           string
		mutate         func(*fastletapi.HeartbeatResponse)
		drainError     bool
		nilDrain       bool
		heartbeatError bool
		nilHeartbeat   bool
		noHeartbeater  bool
		deleted        bool
	}{
		{name: "empty", deleted: true},
		{name: "drain-rpc-error", drainError: true},
		{name: "nil-drain-response", nilDrain: true},
		{name: "missing-heartbeat-client", noHeartbeater: true},
		{name: "heartbeat-rpc-error", heartbeatError: true},
		{name: "nil-heartbeat", nilHeartbeat: true},
		{name: "different-pod", mutate: func(h *fastletapi.HeartbeatResponse) { h.FastletPodUID = "other-pod" }},
		{name: "missing-pod", mutate: func(h *fastletapi.HeartbeatResponse) { h.FastletPodUID = "" }},
		{name: "not-draining", mutate: func(h *fastletapi.HeartbeatResponse) { h.Draining = false }},
		{name: "not-ready", mutate: func(h *fastletapi.HeartbeatResponse) { h.RuntimeReady = false }},
		{name: "recovering", mutate: func(h *fastletapi.HeartbeatResponse) { h.Recovering = true }},
		{name: "used", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Used = 1 }},
		{name: "creating", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Creating = 1 }},
		{name: "running", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Running = 1 }},
		{name: "deleting", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Deleting = 1 }},
		{name: "missing-resources", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Resources = nil }},
		{name: "wrong-resource-version", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Resources.Version = "v1" }},
		{name: "missing-resource-epoch", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Resources.Epoch = "" }},
		{name: "unsupported-resources", mutate: func(h *fastletapi.HeartbeatResponse) {
			h.Admission.Resources.Reason = fastletapi.AdmissionReasonUnsupportedProtocol
		}},
		{name: "invalid-cpu", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Resources.Committed.CPU = "invalid" }},
		{name: "invalid-memory", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Resources.Committed.Memory = "invalid" }},
		{name: "committed-cpu", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Resources.Committed.CPU = "1" }},
		{name: "committed-memory", mutate: func(h *fastletapi.HeartbeatResponse) { h.Admission.Resources.Committed.Memory = "1Gi" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			r, k8s, _, pool := newDrainHarness(t, nil)
			r.Now = func() time.Time { return now }
			pod := getFastletPod(t, k8s, "fastlet-a")
			// Use a non-protected planned replacement past its timeout: legacy
			// verification failures must not fall through to forced deletion.
			pod.Annotations = map[string]string{
				placement.AnnotationDraining: "true", placement.AnnotationDrainID: "drain-b",
				placement.AnnotationDrainReason:    placement.DrainReasonPlannedUpgrade,
				placement.AnnotationDrainStartedAt: now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
			}
			require.NoError(t, k8s.Update(t.Context(), pod))
			heartbeat := emptyLegacyDrainHeartbeat(pod)
			if test.mutate != nil {
				test.mutate(heartbeat)
			}
			if test.nilHeartbeat {
				heartbeat = nil
			}
			acked, heartbeatCalled := false, false
			r.FastletDrainer = &legacyHeartbeatDrainer{
				setDraining: func(request *fastletapi.SetDrainingRequest) (*fastletapi.SetDrainingResponse, error) {
					if test.drainError {
						return nil, errors.New("injected drain RPC failure")
					}
					if test.nilDrain {
						return nil, nil
					}
					acked = true
					return &fastletapi.SetDrainingResponse{Draining: request.Draining}, nil
				},
				heartbeat: func() (*fastletapi.HeartbeatResponse, error) {
					heartbeatCalled = true
					require.True(t, acked, "the heartbeat request must start after SetDraining returns")
					persisted := getFastletPod(t, k8s, pod.Name)
					require.Equal(t, "drain-b", persisted.Annotations[placement.AnnotationDrainAckedID])
					if test.heartbeatError {
						return nil, errors.New("injected heartbeat RPC failure")
					}
					return heartbeat, nil
				},
			}
			if test.noHeartbeater {
				r.FastletDrainer = &recordingDrainer{respond: func(request *fastletapi.SetDrainingRequest) *fastletapi.SetDrainingResponse {
					return &fastletapi.SetDrainingResponse{Draining: request.Draining}
				}}
			}
			_, _, err := r.reconcileDraining(t.Context(), pool, []corev1.Pod{*pod}, nil, 0, "", true, nil, nil)
			require.NoError(t, err)
			require.Equal(t, !test.drainError && !test.nilDrain && !test.noHeartbeater, heartbeatCalled)
			var remaining corev1.Pod
			err = k8s.Get(t.Context(), client.ObjectKeyFromObject(pod), &remaining)
			if test.deleted {
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err, "unverified or loaded legacy state must retain the Pod")
			}
		})
	}
}

func TestLegacyDrainRejectsEarlierAttemptHeartbeatArrivingAfterCurrentAck(t *testing.T) {
	now := time.Now()
	r, k8s, _, pool := newDrainHarness(t, nil)
	r.Now = func() time.Time { return now }
	pod := getFastletPod(t, k8s, "fastlet-a")
	registry := placement.NewInMemoryRegistry()
	info := resourceAwareControllerFastlet(pod.Name, string(pod.UID), 0, fastletapi.AdmissionStateOpen, now)
	seedControllerRegistry(t, registry, info)
	r.Registry = registry
	heartbeat := emptyLegacyDrainHeartbeat(pod)
	heartbeatCalls := 0
	r.FastletDrainer = &legacyHeartbeatDrainer{
		setDraining: func(request *fastletapi.SetDrainingRequest) (*fastletapi.SetDrainingResponse, error) {
			return &fastletapi.SetDrainingResponse{Draining: request.Draining}, nil
		},
		heartbeat: func() (*fastletapi.HeartbeatResponse, error) {
			heartbeatCalls++
			return heartbeat, nil
		},
	}
	require.NoError(t, r.startDrain(t.Context(), pod, placement.DrainReasonPlannedUpgrade))
	drainA := pod.Annotations[placement.AnnotationDrainID]
	delayedA := emptyLegacyDrainHeartbeat(pod)
	delayedA.Sequence, delayedA.Cache = 2, fastletapi.CacheSnapshot{Epoch: "test", Revision: 1}
	require.NoError(t, r.cancelDrain(t.Context(), pod))
	// A different FastPath replica admits work after cancellation; its durable
	// assignment need not have reached Status.Placement or this local Registry.
	heartbeat.Admission.Used, heartbeat.Admission.Running = 1, 1
	heartbeat.Admission.Resources.Committed = fastletapi.SandboxResourceVector{CPU: "1", Memory: "1Gi"}
	now = now.Add(time.Second)
	require.NoError(t, r.startDrain(t.Context(), pod, placement.DrainReasonPlannedUpgrade))
	require.NotEqual(t, drainA, pod.Annotations[placement.AnnotationDrainID])
	ackedAt, err := time.Parse(time.RFC3339Nano, pod.Annotations[placement.AnnotationDrainAckedAt])
	require.NoError(t, err)
	now = now.Add(time.Second)
	require.NoError(t, registry.ApplyHeartbeat(info.ID, info.PodUID, delayedA, now))
	cached, found := registry.GetFastletByID(info.ID)
	require.True(t, found)
	require.True(t, cached.LastHeartbeat.After(ackedAt))
	require.True(t, cached.Draining)
	require.Zero(t, cached.Admission.Used)
	observed := map[string]placement.FastletInfo{podIdentity(pod): cached}
	_, _, err = r.reconcileDraining(t.Context(), pool, []corev1.Pod{*pod}, nil, 0, "", true, nil, observed)
	require.NoError(t, err)
	pod = getFastletPod(t, k8s, pod.Name)
	require.Equal(t, 1, heartbeatCalls, "the delayed Registry response cannot replace a fresh post-ACK read")

	heartbeat = emptyLegacyDrainHeartbeat(pod)
	_, _, err = r.reconcileDraining(t.Context(), pool, []corev1.Pod{*pod}, nil, 0, "", true, nil, observed)
	require.NoError(t, err)
	var gone corev1.Pod
	require.True(t, apierrors.IsNotFound(k8s.Get(t.Context(), client.ObjectKeyFromObject(pod), &gone)))
	require.Equal(t, 2, heartbeatCalls)
}

func TestModernPlannedDrainRequiresAckForTimeoutPolicy(t *testing.T) {
	for _, outcome := range []string{"acknowledged", "rpc-error", "nil-response", "identity-mismatch"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Now()
			r, k8s, _, pool := newDrainHarness(t, nil)
			r.Now = func() time.Time { return now }
			pod := getFastletPod(t, k8s, "fastlet-a")
			pod.Annotations = map[string]string{
				placement.AnnotationDraining: "true", placement.AnnotationDrainID: "drain-b",
				placement.AnnotationDrainReason:    placement.DrainReasonPlannedUpgrade,
				placement.AnnotationDrainStartedAt: now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
			}
			require.NoError(t, k8s.Update(t.Context(), pod))
			r.FastletDrainer = &legacyHeartbeatDrainer{
				setDraining: func(request *fastletapi.SetDrainingRequest) (*fastletapi.SetDrainingResponse, error) {
					if outcome == "rpc-error" {
						return nil, errors.New("injected modern drain RPC failure")
					}
					if outcome == "nil-response" {
						return nil, nil
					}
					response := &fastletapi.SetDrainingResponse{
						Draining: true, DrainID: request.DrainID, FastletPodUID: request.FastletPodUID, RuntimeReady: true,
						Admission: &fastletapi.AdmissionStatus{Used: 1, Running: 1},
					}
					if outcome == "identity-mismatch" {
						response.FastletPodUID = "other-pod"
					}
					return response, nil
				},
				heartbeat: func() (*fastletapi.HeartbeatResponse, error) {
					t.Fatal("modern drain must use its atomic snapshot")
					return nil, nil
				},
			}
			_, _, err := r.reconcileDraining(t.Context(), pool, []corev1.Pod{*pod}, nil, 0, "", false, nil, nil)
			require.NoError(t, err)
			var remaining corev1.Pod
			err = k8s.Get(t.Context(), client.ObjectKeyFromObject(pod), &remaining)
			if outcome == "acknowledged" {
				require.True(t, apierrors.IsNotFound(err), "acknowledged non-protected replacement retains its timeout policy")
			} else {
				require.NoError(t, err, "timeout cannot bypass a failed or invalid ACK")
			}
		})
	}
}
