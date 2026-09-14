package fastpath

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/registryconfig"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestStorePolicySourceResolvesManifestBindings exercises the whole read
// path: pool registry secret -> compiled credential -> store index ->
// manifest -> recorded bindings, with the TTL cache suppressing the second
// resolution.
func TestStorePolicySourceResolvesManifestBindings(t *testing.T) {
	const image = "snap-image-v1"
	manifestBytes, err := artifacts.MarshalManifest(map[string]any{
		"schemaVersion": 1,
		"files":         map[string]any{"rootfs.ext4": map[string]any{"sha256": "x", "sizeBytes": 1}},
		"actionBindings": []map[string]string{
			{"handler": "egress", "input": `{"deny":true}`},
			{"handler": "audit", "input": `{}`},
		},
	})
	require.NoError(t, err)
	digest := artifacts.SHA256Of(manifestBytes)
	digest16 := artifacts.Digest16(manifestBytes)
	indexBytes, err := artifacts.ImageIndexPayload(image,
		"s3://sandbox-images/publish/"+digest16+"/manifest.json", digest, time.Now())
	require.NoError(t, err)

	var requests atomic.Int64
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		key := strings.TrimPrefix(r.URL.Path, "/sandbox-images/publish/")
		switch key {
		case "index/" + artifacts.ImageIndexKey(image) + ".json":
			_, _ = w.Write(indexBytes)
		case digest16 + "/manifest.json":
			_, _ = w.Write(manifestBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer store.Close()

	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: "store.example.com", Username: "reader", Password: "reader-secret", Endpoint: store.URL,
	}})
	require.NoError(t, err)
	secretData, err := compiled.Marshal()
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: poolRegistrySecretName("pool-a"), Namespace: "default"},
		Data:       map[string][]byte{registryconfig.SecretKey: secretData},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, secret).Build()

	source := NewStorePolicySource(k8sClient, "s3://sandbox-images/publish", "")
	bindings, err := source.ActionBindings(context.Background(), pool, image)
	require.NoError(t, err)
	require.Equal(t, []apiv1alpha2.ActionBinding{
		{Handler: "egress", Input: `{"deny":true}`},
		{Handler: "audit", Input: `{}`},
	}, bindings)

	// The TTL cache serves the second call without touching the store.
	again, err := source.ActionBindings(context.Background(), pool, image)
	require.NoError(t, err)
	require.Equal(t, bindings, again)
	require.Equal(t, int64(2), requests.Load(), "index + manifest fetched exactly once")
}

// TestStorePolicySourceEndpointOverride mirrors the real deployment: the
// pool-compiled credential carries no endpoint (the registry rule has
// none), so the client must take the endpoint from the controller's
// override — without it the default https:// against a plain-HTTP store
// fails and policy resolution silently degrades.
func TestStorePolicySourceEndpointOverride(t *testing.T) {
	const image = "snap-image-v1"
	manifestBytes, err := artifacts.MarshalManifest(map[string]any{
		"schemaVersion":  1,
		"files":          map[string]any{},
		"actionBindings": []map[string]string{{"handler": "egress", "input": `{"deny":true}`}},
	})
	require.NoError(t, err)
	digest := artifacts.SHA256Of(manifestBytes)
	digest16 := artifacts.Digest16(manifestBytes)
	indexBytes, err := artifacts.ImageIndexPayload(image,
		"s3://sandbox-images/publish/"+digest16+"/manifest.json", digest, time.Now())
	require.NoError(t, err)

	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/sandbox-images/publish/")
		switch key {
		case "index/" + artifacts.ImageIndexKey(image) + ".json":
			_, _ = w.Write(indexBytes)
		case digest16 + "/manifest.json":
			_, _ = w.Write(manifestBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer store.Close()

	// No Endpoint on the credential — only the host, like the pool-compiled
	// secret.
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: "172.28.0.4:9000", Username: "reader", Password: "reader-secret",
	}})
	require.NoError(t, err)
	secretData, err := compiled.Marshal()
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: poolRegistrySecretName("pool-a"), Namespace: "default"},
		Data:       map[string][]byte{registryconfig.SecretKey: secretData},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, secret).Build()

	source := NewStorePolicySource(k8sClient, "s3://sandbox-images/publish", store.URL)
	bindings, err := source.ActionBindings(context.Background(), pool, image)
	require.NoError(t, err)
	require.Equal(t, []apiv1alpha2.ActionBinding{{Handler: "egress", Input: `{"deny":true}`}}, bindings)
}

func TestStorePolicySourceManifestWithoutBindings(t *testing.T) {
	const image = "golden-image-v1"
	manifestBytes, err := artifacts.MarshalManifest(map[string]any{"schemaVersion": 1, "files": map[string]any{}})
	require.NoError(t, err)
	digest := artifacts.SHA256Of(manifestBytes)
	digest16 := artifacts.Digest16(manifestBytes)
	indexBytes, err := artifacts.ImageIndexPayload(image,
		"s3://sandbox-images/publish/"+digest16+"/manifest.json", digest, time.Now())
	require.NoError(t, err)

	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/sandbox-images/publish/")
		switch key {
		case "index/" + artifacts.ImageIndexKey(image) + ".json":
			_, _ = w.Write(indexBytes)
		case digest16 + "/manifest.json":
			_, _ = w.Write(manifestBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer store.Close()

	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: "store.example.com", Username: "reader", Password: "reader-secret", Endpoint: store.URL,
	}})
	require.NoError(t, err)
	secretData, err := compiled.Marshal()
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: poolRegistrySecretName("pool-a"), Namespace: "default"},
		Data:       map[string][]byte{registryconfig.SecretKey: secretData},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, secret).Build()

	source := NewStorePolicySource(k8sClient, "s3://sandbox-images/publish", "")
	bindings, err := source.ActionBindings(context.Background(), pool, image)
	require.NoError(t, err)
	require.Empty(t, bindings, "a golden-image manifest carries no recorded policy")
}
