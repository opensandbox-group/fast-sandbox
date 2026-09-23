package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifactstore"
	"fast-sandbox/internal/registryconfig"
)

// fakeProvider mocks the registryconfig.Provider interface.
type fakeProvider struct {
	credential registryconfig.Credential
	found      bool
	err        error
	lastRef    string
}

func (f *fakeProvider) Credentials(reference string) (registryconfig.Credential, bool, error) {
	f.lastRef = reference
	return f.credential, f.found, f.err
}

func (f *fakeProvider) Revision() string { return "" }

func TestResolveCredentialMatchesStoreHost(t *testing.T) {
	provider := &fakeProvider{
		credential: registryconfig.Credential{
			Host: "oss-cn-hangzhou.aliyuncs.com", Username: "readonly-ak", Password: "readonly-sk",
		},
		found: true,
	}
	credential, err := resolveCredential(provider, "s3://oss-cn-hangzhou.aliyuncs.com/sandbox-images/publish", "")
	require.NoError(t, err)
	require.Equal(t, "readonly-ak", credential.Username)
	require.Equal(t, "readonly-sk", credential.Password)
	// The provider is matched against the store endpoint host.
	require.Equal(t, "oss-cn-hangzhou.aliyuncs.com", provider.lastRef)
}

func TestResolveCredentialMatchesExplicitEndpoint(t *testing.T) {
	provider := &fakeProvider{
		credential: registryconfig.Credential{
			Host: "127.0.0.1:9000", Username: "chain-test", Password: "chain-test-secret",
			Endpoint: "http://127.0.0.1:9000",
		},
		found: true,
	}
	credential, err := resolveCredential(provider, "s3://sandbox-images/publish", "http://127.0.0.1:9000")
	require.NoError(t, err)
	require.Equal(t, "chain-test", credential.Username)
	require.Equal(t, "http://127.0.0.1:9000", credential.Endpoint)
	require.Equal(t, "127.0.0.1:9000", provider.lastRef)
}

// TestResolveCredentialHostMatchAgainstFile exercises the real compiled
// configuration through FileProvider: a bare endpoint host (no "/") must
// match by host, not by the image-reference rules of Match (which would
// parse "127.0.0.1:9000" as repository "127.0.0.1:9000" under docker.io).
func TestResolveCredentialHostMatchAgainstFile(t *testing.T) {
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: "127.0.0.1:9000", Username: "chain-test", Password: "chain-test-secret",
		Endpoint: "http://127.0.0.1:9000",
	}})
	require.NoError(t, err)
	payload, err := compiled.Marshal()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "registry.json")
	require.NoError(t, os.WriteFile(path, payload, 0o640))

	provider := registryconfig.NewFileProvider(path)
	credential, err := resolveCredential(provider, "s3://sandbox-images/publish", "http://127.0.0.1:9000")
	require.NoError(t, err)
	require.Equal(t, "chain-test", credential.Username)
	require.Equal(t, "chain-test-secret", credential.Password)
	require.Equal(t, "http://127.0.0.1:9000", credential.Endpoint)
}

func TestResolveCredentialInvalidEndpoint(t *testing.T) {
	provider := &fakeProvider{found: true}
	_, err := resolveCredential(provider, "s3://sandbox-images/publish", "://not-a-url")
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint")
}

func TestResolveCredentialNoMatch(t *testing.T) {
	provider := &fakeProvider{found: false}
	_, err := resolveCredential(provider, "s3://sandbox-images/publish", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no read-only credential")
}

func TestResolveCredentialProviderError(t *testing.T) {
	provider := &fakeProvider{err: errors.New("config unreadable")}
	_, err := resolveCredential(provider, "s3://sandbox-images/publish", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "config unreadable")
}

func TestResolveCredentialInvalidStoreRoot(t *testing.T) {
	provider := &fakeProvider{found: true}
	_, err := resolveCredential(provider, "not a url ://", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "store root")
}

func TestResolveCredentialMissingHost(t *testing.T) {
	provider := &fakeProvider{found: true}
	_, err := resolveCredential(provider, "s3:///only-path", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint host")
}

// TestLivePullClientRebuildsOnConfigChange pins the no-restart contract: the
// mounted store files and the registry file are re-read per current() call,
// and a changed store/endpoint/credential rebuilds the pull client instead of
// reusing the old one.
func TestLivePullClientRebuildsOnConfigChange(t *testing.T) {
	storeDir := t.TempDir()
	writeStore := func(store, endpoint string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(storeDir, artifactstore.StoreKey), []byte(store), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(storeDir, artifactstore.EndpointKey), []byte(endpoint), 0o644))
	}
	registryPath := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry := func(host, username, password string) {
		t.Helper()
		compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
			Host: host, Username: username, Password: password,
		}})
		require.NoError(t, err)
		payload, err := compiled.Marshal()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(registryPath, payload, 0o640))
	}

	writeStore("s3://bucket/publish", "http://minio-1:9000")
	writeRegistry("minio-1:9000", "reader", "secret")
	client := &livePullClient{
		config:   artifactstore.Loader{Dir: storeDir},
		registry: registryconfig.NewFileProvider(registryPath),
	}

	first, err := client.current()
	require.NoError(t, err)
	again, err := client.current()
	require.NoError(t, err)
	require.Same(t, first, again, "unchanged config must reuse the pull client")

	// ConfigMap edit + credential rotation, both observed without restart
	// (the same FileProvider re-reads the rewritten registry file).
	writeStore("s3://bucket/publish", "http://minio-2:9000")
	writeRegistry("minio-2:9000", "reader", "rotated-secret")

	second, err := client.current()
	require.NoError(t, err)
	require.NotSame(t, first, second, "a config change must rebuild the pull client")
}

func TestLivePullClientUnconfiguredStore(t *testing.T) {
	client := &livePullClient{
		config:   artifactstore.Loader{Dir: filepath.Join(t.TempDir(), "absent")},
		registry: registryconfig.NewFileProvider(filepath.Join(t.TempDir(), "absent.json")),
	}
	_, err := client.current()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not configured")
}

// TestGatewayProberClassifiesStatuses pins the external-gateway health
// verdict: any non-5xx answer proves the gateway serves; 5xx and transport
// errors count as down (pulls would be on the direct-S3 fallback).
func TestGatewayProberClassifiesStatuses(t *testing.T) {
	var status atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(server.Close)

	prober := newGatewayProber(server.URL)
	prober.base = server.URL
	status.Store(http.StatusOK)
	prober.probe()
	require.True(t, prober.Healthy(), "200 must count as up")

	status.Store(http.StatusNotFound)
	prober.probe()
	require.True(t, prober.Healthy(), "4xx on the bare base URL still proves the gateway serves")

	status.Store(http.StatusBadGateway)
	prober.probe()
	require.False(t, prober.Healthy(), "5xx must count as down")

	prober.base = "http://127.0.0.1:1"
	prober.probe()
	require.False(t, prober.Healthy(), "transport errors must count as down")
}

// TestGatewayProberCachesVerdict pins the non-blocking contract: Healthy
// reads the cached verdict without touching the gateway.
func TestGatewayProberCachesVerdict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	prober := newGatewayProber(server.URL)
	require.False(t, prober.Healthy(), "no probe yet")
	prober.probe()
	server.Close() // the gateway disappears...
	require.True(t, prober.Healthy(), "Healthy must serve the cached verdict, not re-probe")
}
