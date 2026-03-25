package basicvalidation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestPortValidation(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("port-validation").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "smoke").
		Assess("reject invalid ports and schedule valid boundary ports", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("port")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := &apiv1alpha1.SandboxPool{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "SandboxPool",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "port-validation-pool",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxPoolSpec{
					Capacity: apiv1alpha1.PoolCapacity{
						PoolMin: 1,
						PoolMax: 3, // Increased for multiple valid port tests
					},
					MaxSandboxesPerPod: 10,
					RuntimeType:        apiv1alpha1.RuntimeContainer,
					AgentTemplate: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "agent",
								Image: suiteenv.AgentImage(),
							}},
						},
					},
				},
			}
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}
			if _, err := fixture.WaitForReadyAgentPods(ctx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			invalidCases := []struct {
				name        string
				port        int32
				wantMessage []string
			}{
				{
					name:        "zero",
					port:        0,
					wantMessage: []string{"spec.exposedPorts[0]", "greater than or equal to 1"},
				},
				{
					name:        "over-max",
					port:        65536,
					wantMessage: []string{"spec.exposedPorts[0]", "less than or equal to 65535"},
				},
			}
			for _, tc := range invalidCases {
				sandbox := portValidationSandbox(namespace, fmt.Sprintf("invalid-port-%s", tc.name), pool.Name, tc.port)
				err := k8sClient.Create(ctx, sandbox)
				if err == nil {
					t.Fatalf("create invalid sandbox for port %d: got nil error", tc.port)
				}
				statusErr, ok := err.(*apierrors.StatusError)
				if !ok {
					t.Fatalf("create invalid sandbox for port %d: error type %T, want *apierrors.StatusError", tc.port, err)
				}
				message := statusErr.ErrStatus.Message
				for _, want := range tc.wantMessage {
					if !strings.Contains(message, want) {
						t.Fatalf("create invalid sandbox for port %d: error %q, want substring %q", tc.port, message, want)
					}
				}
			}

			validPorts := []int32{5758, 65535}
			for _, port := range validPorts {
				sandbox := portValidationSandbox(namespace, fmt.Sprintf("valid-port-%d", port), pool.Name, port)
				if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
					t.Fatalf("create valid sandbox for port %d: %v", port, err)
				}
				if _, err := fixture.WaitForSandbox(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
					return sb.Status.AssignedPod != "" &&
						(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
				}); err != nil {
					t.Fatalf("wait for valid sandbox assignment on port %d: %v", port, err)
				}
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func portValidationSandbox(namespace, name, pool string, port int32) *apiv1alpha1.Sandbox {
	return &apiv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:        "docker.io/library/alpine:latest",
			Command:      []string{"/bin/sleep", "60"},
			PoolRef:      pool,
			ExposedPorts: []int32{port},
		},
	}
}
