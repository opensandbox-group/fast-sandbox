package reconciler

import (
	"context"
	"encoding/base64"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"fast-sandbox/internal/registryconfig"
)

func registryCompileClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func registryConfigMap(namespace, body string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: registryconfig.ConfigMapName, Namespace: namespace},
		Data:       map[string]string{registryconfig.ConfigMapKey: body},
	}
}

func dockerconfigSecret(namespace, name, host, user, pass string) *corev1.Secret {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	payload := `{"auths":{"` + host + `":{"username":"` + user + `","password":"` + pass + `","auth":"` + auth + `"}}}`
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(payload)},
	}
}

func TestCompileRegistryConfigAbsentConfigMapIsEmpty(t *testing.T) {
	c := registryCompileClient(t)
	compiled, err := compileRegistryConfig(context.Background(), c, "tenant-a")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(compiled.Credentials) != 0 {
		t.Fatalf("expected empty credentials without a ConfigMap, got %d", len(compiled.Credentials))
	}
	empty, err := registryconfig.NewCompiled(nil)
	if err != nil {
		t.Fatalf("new compiled: %v", err)
	}
	if compiled.Revision != empty.Revision {
		t.Fatalf("expected empty revision %q, got %q", empty.Revision, compiled.Revision)
	}
}

func TestCompileRegistryConfigResolvesCredential(t *testing.T) {
	ns := "tenant-a"
	body := "registries:\n  - host: jd-registry.example.com\n    secretRef:\n      name: pull-creds\n"
	c := registryCompileClient(t,
		registryConfigMap(ns, body),
		dockerconfigSecret(ns, "pull-creds", "jd-registry.example.com", "u", "p"),
	)
	compiled, err := compileRegistryConfig(context.Background(), c, ns)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cred, found := compiled.Match("jd-registry.example.com/system:python_3.12")
	if !found {
		t.Fatal("expected a credential match for the configured host")
	}
	if cred.Username != "u" || cred.Password != "p" {
		t.Fatalf("expected u/p, got %+v", cred)
	}
}

func TestCompileRegistryConfigRejectsNonDockerconfigSecret(t *testing.T) {
	ns := "tenant-a"
	body := "registries:\n  - host: jd-registry.example.com\n    secretRef:\n      name: pull-creds\n"
	badSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-creds", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"foo": []byte("bar")},
	}
	c := registryCompileClient(t, registryConfigMap(ns, body), badSecret)
	if _, err := compileRegistryConfig(context.Background(), c, ns); err == nil {
		t.Fatal("expected an error for a non-dockerconfigjson secret")
	}
}

func TestCompileRegistryConfigRejectsMissingHostCredential(t *testing.T) {
	ns := "tenant-a"
	body := "registries:\n  - host: jd-registry.example.com\n    secretRef:\n      name: pull-creds\n"
	// Secret present but carries credentials for a different host.
	c := registryCompileClient(t,
		registryConfigMap(ns, body),
		dockerconfigSecret(ns, "pull-creds", "other-registry.example.com", "u", "p"),
	)
	if _, err := compileRegistryConfig(context.Background(), c, ns); err == nil {
		t.Fatal("expected an error when the secret has no credentials for the rule host")
	}
}
