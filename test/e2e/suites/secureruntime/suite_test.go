package secureruntime

import (
	"context"
	"os"
	"testing"

	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/conf"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/suiteenv"
)

var testSuite = suiteenv.New()

func TestMain(m *testing.M) {
	os.Exit(testSuite.Env().Run(m))
}

// SecureRuntimeTestClient provides helpers for secure runtime tests.
type SecureRuntimeTestClient struct {
	client client.Client
	scheme *runtime.Scheme
}

// MustSecureRuntimeClient creates a test client with RuntimeClass support.
func MustSecureRuntimeClient(t *testing.T) *SecureRuntimeTestClient {
	t.Helper()

	cfg, err := conf.New(testSuite.Config().KubeconfigFile())
	if err != nil {
		t.Fatalf("resolve kubeconfig: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := apiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add fast-sandbox scheme: %v", err)
	}
	// Add RuntimeClass scheme
	scheme.AddKnownTypes(schema.GroupVersion{Group: "node.k8s.io", Version: "v1"}, &nodev1.RuntimeClass{}, &nodev1.RuntimeClassList{})

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create kube client: %v", err)
	}

	return &SecureRuntimeTestClient{client: k8sClient, scheme: scheme}
}

// RuntimeClassExists checks if a RuntimeClass exists.
func (c *SecureRuntimeTestClient) RuntimeClassExists(ctx context.Context, name string) (bool, error) {
	runtimeClass := &nodev1.RuntimeClass{}
	err := c.client.Get(ctx, client.ObjectKey{Name: name}, runtimeClass)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SkipIfRuntimeClassNotExists skips the test if RuntimeClass doesn't exist.
func (c *SecureRuntimeTestClient) SkipIfRuntimeClassNotExists(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	exists, err := c.RuntimeClassExists(ctx, name)
	if err != nil {
		t.Fatalf("check RuntimeClass: %v", err)
	}
	if !exists {
		t.Skipf("RuntimeClass %q not found, skipping test", name)
	}
}

func (c *SecureRuntimeTestClient) Client() client.Client {
	return c.client
}
