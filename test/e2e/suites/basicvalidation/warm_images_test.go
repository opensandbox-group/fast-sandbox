package basicvalidation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/fastlet/cache"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestPoolWarmImagesReachRuntimeCacheInventory(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("pool-warm-images").
		WithLabel("suite", "basic-validation").
		WithLabel("tier", "smoke").
		Assess("Fastlet asynchronously prepares Pool warmImages and reports the real runtime inventory", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// The e2e environment explicitly loads alpine:latest into the kind
			// node. This still exercises RuntimeArtifactCache.PullImage and the
			// real containerd inventory without coupling the Gate to Docker Hub
			// reachability or rate limits.
			const warmImage = "docker.io/library/alpine:latest"
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("warm-images")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := warmImagePool(namespace, warmImage)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create warm image Pool: %v", err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(waitCtx, types.NamespacedName{Namespace: namespace, Name: pool.Name}, 1); err != nil {
				t.Fatalf("wait for Ready Fastlet before warm image completion: %v", err)
			}

			var pods corev1.PodList
			if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool.Name}); err != nil {
				t.Fatalf("list warm image Fastlet: %v", err)
			}
			if len(pods.Items) != 1 {
				t.Fatalf("Fastlet Pod count=%d, want 1", len(pods.Items))
			}
			baseURL, forward, err := e2eenv.StartPodPortForward(ctx, namespace, pods.Items[0].Name, 5758)
			if err != nil {
				t.Fatalf("port-forward Fastlet control API: %v", err)
			}
			defer func() {
				if cleanupErr := forward.Cleanup(); cleanupErr != nil {
					t.Errorf("cleanup Fastlet port-forward: %v", cleanupErr)
				}
			}()

			target := cache.NormalizeReference(warmImage)
			var lastErr error
			for {
				ready, checkErr := warmImagePrepared(waitCtx, baseURL, target)
				if ready {
					return ctx
				}
				if checkErr != nil {
					lastErr = checkErr
				}
				select {
				case <-waitCtx.Done():
					t.Fatalf("warm image %q never reached complete runtime cache inventory with a successful preparation metric: %v (last error: %v)", warmImage, waitCtx.Err(), lastErr)
				case <-time.After(500 * time.Millisecond):
				}
			}
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func warmImagePool(namespace, image string) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha1.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "warm-image-pool", Namespace: namespace},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity:           apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 1},
			MaxSandboxesPerPod: 1,
			Runtime:            apiv1alpha1.RuntimeContainer,
			SandboxResources:   suiteenv.SmallSandboxResourceProfile(),
			WarmImages:         []string{image},
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "fastlet", Image: suiteenv.FastletImage(), ImagePullPolicy: corev1.PullIfNotPresent,
			}}}},
		},
	}
}

func warmImagePrepared(ctx context.Context, baseURL, target string) (bool, error) {
	heartbeatRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v2/fastlet/heartbeat?fullCache=true", nil)
	if err != nil {
		return false, err
	}
	heartbeatResponse, err := http.DefaultClient.Do(heartbeatRequest)
	if err != nil {
		return false, err
	}
	defer heartbeatResponse.Body.Close()
	if heartbeatResponse.StatusCode != http.StatusOK {
		return false, fmt.Errorf("heartbeat status=%d", heartbeatResponse.StatusCode)
	}
	var heartbeat fastletapi.HeartbeatResponse
	if err := json.NewDecoder(heartbeatResponse.Body).Decode(&heartbeat); err != nil {
		return false, err
	}
	if !heartbeat.Cache.Complete || !containsWarmImage(heartbeat.Cache.Images, target) {
		return false, nil
	}

	metricsRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/metrics", nil)
	if err != nil {
		return false, err
	}
	metricsResponse, err := http.DefaultClient.Do(metricsRequest)
	if err != nil {
		return false, err
	}
	defer metricsResponse.Body.Close()
	if metricsResponse.StatusCode != http.StatusOK {
		return false, fmt.Errorf("metrics status=%d", metricsResponse.StatusCode)
	}
	body, err := io.ReadAll(metricsResponse.Body)
	if err != nil {
		return false, err
	}
	return successfulWarmImagePulls(string(body)) > 0, nil
}

func containsWarmImage(images []string, target string) bool {
	for _, image := range images {
		if image == target {
			return true
		}
	}
	return false
}

func successfulWarmImagePulls(metrics string) float64 {
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, `fast_sandbox_warm_image_pull_total{result="success"}`) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0
		}
		value, _ := strconv.ParseFloat(fields[1], 64)
		return value
	}
	return 0
}
