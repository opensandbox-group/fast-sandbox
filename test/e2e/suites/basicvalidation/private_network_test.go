package basicvalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type networkSlotState struct {
	ID        string `json:"id"`
	Phase     string `json:"phase"`
	NetNSName string `json:"netnsName"`
	IP        string `json:"ip"`
	Owner     struct {
		SandboxUID string `json:"sandboxUid"`
	} `json:"owner"`
}

func TestSandboxPrivateNetwork(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("sandbox-private-network").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "network").
		Assess("same-port sandboxes are isolated and network state survives Fastlet restart", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("private-network")
			createNamespace(ctx, t, k8sClient, namespace)
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := privateNetworkPool(namespace, "private-network-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create private network pool: %v", err)
			}
			waitForPoolReady(ctx, t, fixture, namespace, pool.Name)

			first := privateNetworkSandbox(namespace, "private-network-a", pool.Name)
			second := privateNetworkSandbox(namespace, "private-network-b", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, first); err != nil {
				t.Fatalf("create first sandbox: %v", err)
			}
			if _, err := fixture.CreateSandbox(ctx, namespace, second); err != nil {
				t.Fatalf("create second sandbox: %v", err)
			}
			first = waitForAssignedSandbox(ctx, t, fixture, namespace, first.Name)
			second = waitForAssignedSandbox(ctx, t, fixture, namespace, second.Name)
			if first.Status.AssignedFastlet != second.Status.AssignedFastlet {
				t.Fatalf("sandboxes were not placed on the same Fastlet: %s != %s", first.Status.AssignedFastlet, second.Status.AssignedFastlet)
			}
			fastletPod := first.Status.AssignedFastlet
			if first.Status.Assignment == nil || second.Status.Assignment == nil {
				t.Fatalf("sandboxes have no authoritative assignment")
			}
			fastletPodUID := first.Status.Assignment.FastletPodUID
			if fastletPodUID == "" || second.Status.Assignment.FastletPodUID != fastletPodUID {
				t.Fatalf("sandboxes do not share a fenced Fastlet Pod UID")
			}

			waitForSandboxLog(ctx, t, namespace, fastletPod, sandboxIdentifier(first), "DNS_OK")
			waitForSandboxLog(ctx, t, namespace, fastletPod, sandboxIdentifier(second), "DNS_OK")
			states := waitForNetworkStates(ctx, t, namespace, fastletPod, fastletPodUID, sandboxIdentifier(first), sandboxIdentifier(second))
			firstState := states[sandboxIdentifier(first)]
			secondState := states[sandboxIdentifier(second)]
			if firstState.IP == secondState.IP {
				t.Fatalf("sandboxes share private IP %s", firstState.IP)
			}

			waitForSandboxHTTP(ctx, t, namespace, fastletPod, firstState.IP, first.Name)
			waitForSandboxHTTP(ctx, t, namespace, fastletPod, secondState.IP, second.Name)
			assertSandboxCannotReach(ctx, t, namespace, fastletPod, firstState.NetNSName, secondState.IP)
			assertSandboxCannotReach(ctx, t, namespace, fastletPod, secondState.NetNSName, firstState.IP)

			if err := k8sClient.Delete(ctx, first); err != nil {
				t.Fatalf("delete first sandbox: %v", err)
			}
			deleteCtx, cancelDelete := context.WithTimeout(ctx, 90*time.Second)
			defer cancelDelete()
			if err := fixture.WaitForSandboxDeleted(deleteCtx, types.NamespacedName{Namespace: namespace, Name: first.Name}); err != nil {
				t.Fatalf("wait for first sandbox deletion: %v", err)
			}
			waitForNetworkStateAbsent(ctx, t, namespace, fastletPod, fastletPodUID, sandboxIdentifier(first))
			waitForSandboxHTTP(ctx, t, namespace, fastletPod, secondState.IP, second.Name)

			initialRestartCount := fastletRestartCount(ctx, t, k8sClient, namespace, fastletPod)
			_, _ = kubectl(ctx, "exec", "-n", namespace, fastletPod, "-c", "fastlet", "--", "kill", "1")
			waitForFastletRestart(ctx, t, k8sClient, namespace, fastletPod, fastletPodUID, initialRestartCount)
			states = waitForNetworkStates(ctx, t, namespace, fastletPod, fastletPodUID, sandboxIdentifier(second))
			recovered := states[sandboxIdentifier(second)]
			if recovered.ID != secondState.ID || recovered.IP != secondState.IP || recovered.NetNSName != secondState.NetNSName {
				t.Fatalf("network descriptor changed across Fastlet restart: before=%+v after=%+v", secondState, recovered)
			}
			waitForSandboxHTTP(ctx, t, namespace, fastletPod, recovered.IP, second.Name)
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func privateNetworkPool(namespace, name string) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha1.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity: apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 1}, MaxSandboxesPerPod: 2,
			Runtime: apiv1alpha1.RuntimeContainer,
			SandboxResources: apiv1alpha1.SandboxResourceProfile{
				CPU: resource.MustParse("50m"), Memory: resource.MustParse("64Mi"), PIDs: 64,
			},
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "fastlet", Image: suiteenv.FastletImage(),
			}}}},
		},
	}
}

func privateNetworkSandbox(namespace, name, pool string) *apiv1alpha1.Sandbox {
	command := fmt.Sprintf(
		`if nslookup kubernetes.default.svc.cluster.local; then echo DNS_OK; else echo DNS_FAIL; fi; printf '%%s\n' '#!/bin/sh' 'printf "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n%s\n"' > /serve.sh; chmod +x /serve.sh; exec nc -lk -p 8080 -e /serve.sh`,
		name,
	)
	return &apiv1alpha1.Sandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha1.GroupVersion.String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: apiv1alpha1.SandboxSpec{
			Image: "docker.io/library/alpine:latest", PoolRef: pool,
			Command: []string{"/bin/sh", "-c", command},
		},
	}
}

func waitForNetworkStates(ctx context.Context, t *testing.T, namespace, pod, podUID string, sandboxIDs ...string) map[string]networkSlotState {
	t.Helper()
	wanted := make(map[string]struct{}, len(sandboxIDs))
	for _, sandboxID := range sandboxIDs {
		wanted[sandboxID] = struct{}{}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var latest map[string]networkSlotState
	for {
		states, err := readNetworkStates(waitCtx, namespace, pod, podUID)
		if err == nil {
			latest = states
			complete := true
			for sandboxID := range wanted {
				state, exists := states[sandboxID]
				if !exists || state.Phase != "Bound" || state.IP == "" || state.NetNSName == "" {
					complete = false
					break
				}
			}
			if complete {
				return states
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for network states %v: %v; latest=%+v", sandboxIDs, waitCtx.Err(), latest)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func waitForNetworkStateAbsent(ctx context.Context, t *testing.T, namespace, pod, podUID, sandboxID string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		states, err := readNetworkStates(waitCtx, namespace, pod, podUID)
		if err == nil {
			if _, exists := states[sandboxID]; !exists {
				return
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("network state for sandbox %s was not removed: %v", sandboxID, waitCtx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func readNetworkStates(ctx context.Context, namespace, pod, podUID string) (map[string]networkSlotState, error) {
	command := fmt.Sprintf(`for f in /run/fast-sandbox/network/%s/*.json; do [ -f "$f" ] && cat "$f" && echo; done`, podUID)
	output, err := kubectl(ctx, "exec", "-n", namespace, pod, "-c", "fastlet", "--", "sh", "-c", command)
	if err != nil {
		return nil, err
	}
	states := make(map[string]networkSlotState)
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var state networkSlotState
		if err := decoder.Decode(&state); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if state.Owner.SandboxUID != "" {
			states[state.Owner.SandboxUID] = state
		}
	}
	return states, nil
}

func waitForSandboxHTTP(ctx context.Context, t *testing.T, namespace, pod, ip, want string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var last string
	for {
		output, err := kubectl(waitCtx, "exec", "-n", namespace, pod, "-c", "fastlet", "--", "wget", "-q", "-O", "-", "http://"+ip+":8080/")
		last = string(output)
		if err == nil && strings.Contains(last, want) {
			return
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for Sandbox HTTP %s:8080 containing %q: %v; last=%q", ip, want, waitCtx.Err(), last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func assertSandboxCannotReach(ctx context.Context, t *testing.T, namespace, pod, netns, peerIP string) {
	t.Helper()
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := kubectl(checkCtx, "exec", "-n", namespace, pod, "-c", "fastlet", "--",
		"ip", "netns", "exec", netns, "wget", "-q", "-O", "-", "http://"+peerIP+":8080/")
	if err == nil {
		t.Fatalf("sandbox netns %s unexpectedly reached peer %s: %q", netns, peerIP, string(output))
	}
}

func fastletRestartCount(ctx context.Context, t *testing.T, kubeClient ctrlclient.Client, namespace, pod string) int32 {
	t.Helper()
	current := &corev1.Pod{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pod}, current); err != nil {
		t.Fatalf("get Fastlet Pod: %v", err)
	}
	for _, status := range current.Status.ContainerStatuses {
		if status.Name == "fastlet" {
			return status.RestartCount
		}
	}
	t.Fatalf("Fastlet container status not found")
	return 0
}

func waitForFastletRestart(ctx context.Context, t *testing.T, kubeClient ctrlclient.Client, namespace, pod, podUID string, previous int32) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		current := &corev1.Pod{}
		if err := kubeClient.Get(waitCtx, types.NamespacedName{Namespace: namespace, Name: pod}, current); err == nil &&
			string(current.UID) == podUID && isReady(current) {
			for _, status := range current.Status.ContainerStatuses {
				if status.Name == "fastlet" && status.RestartCount > previous && status.Ready {
					return
				}
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for Fastlet container restart: %v", waitCtx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func isReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func kubectl(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "kubectl", args...)
	return command.CombinedOutput()
}
