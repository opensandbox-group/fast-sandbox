package basicvalidation

import (
	"context"
	"strings"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestSandboxCRDValidation(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("sandbox-crd-validation").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "smoke").
		Assess("reject invalid sandbox specs and accept a valid spec", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			namespace := testSuite.AllocateNamespace("validation")

			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			cases := []struct {
				name        string
				sandboxName string
				spec        map[string]any
				wantErr     []string
			}{
				{
					name:        "missing image",
					sandboxName: "test-no-image",
					spec:        map[string]any{"poolRef": "test-pool"},
					wantErr:     []string{"image", "Required"},
				},
				{
					name:        "missing poolRef",
					sandboxName: "test-no-poolref",
					spec:        map[string]any{"image": "nginx:alpine"},
					wantErr:     []string{"poolRef", "Required"},
				},
				{
					name:        "empty image",
					sandboxName: "test-empty-image",
					spec:        map[string]any{"image": "", "poolRef": "test-pool"},
					wantErr:     []string{"image", "at least 1 chars"},
				},
				{
					name:        "empty poolRef",
					sandboxName: "test-empty-poolref",
					spec:        map[string]any{"image": "nginx:alpine", "poolRef": ""},
					wantErr:     []string{"poolRef", "at least 1 chars"},
				},
				{
					name:        "invalid failurePolicy",
					sandboxName: "test-invalid-failure-policy",
					spec: map[string]any{
						"image":         "nginx:alpine",
						"poolRef":       "test-pool",
						"failurePolicy": "InvalidPolicy",
					},
					wantErr: []string{"failurePolicy", "Unsupported value"},
				},
				{
					name:        "env missing name",
					sandboxName: "test-env-no-name",
					spec: map[string]any{
						"image":   "nginx:alpine",
						"poolRef": "test-pool",
						"envs": []any{
							map[string]any{"value": "test-value"},
						},
					},
					wantErr: []string{"envs", "name", "Required"},
				},
			}

			for _, tc := range cases {
				sb := sandboxObject(namespace, tc.sandboxName, tc.spec)
				err := k8sClient.Create(ctx, sb)
				if err == nil {
					t.Fatalf("Create(%s) error = nil, want validation failure", tc.name)
				}
				statusErr, ok := err.(*apierrors.StatusError)
				if !ok {
					t.Fatalf("Create(%s) error type = %T, want *apierrors.StatusError", tc.name, err)
				}
				message := statusErr.ErrStatus.Message
				for _, want := range tc.wantErr {
					if !strings.Contains(message, want) {
						t.Fatalf("Create(%s) error = %q, want substring %q", tc.name, message, want)
					}
				}
			}

			validSandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-valid-sandbox",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxSpec{
					Image:                  "nginx:alpine",
					PoolRef:                "test-pool",
					ExposedPorts:           []int32{8080},
					FailurePolicy:          apiv1alpha1.FailurePolicyManual,
					RecoveryTimeoutSeconds: 60,
				},
			}
			if err := k8sClient.Create(ctx, validSandbox); err != nil {
				t.Fatalf("create valid sandbox: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func sandboxObject(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	sb := &unstructured.Unstructured{}
	sb.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   apiv1alpha1.GroupVersion.Group,
		Version: apiv1alpha1.GroupVersion.Version,
		Kind:    "Sandbox",
	})
	sb.SetNamespace(namespace)
	sb.SetName(name)
	sb.Object["spec"] = spec
	return sb
}
