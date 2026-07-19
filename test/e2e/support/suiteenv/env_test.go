package suiteenv

import (
	"context"
	"errors"
	"testing"

	e2eenv "fast-sandbox/test/e2e/env"
)

func TestNewUsesExplicitKubeconfig(t *testing.T) {
	env := New(WithKubeconfig("/tmp/test-kubeconfig"))

	if got := env.Config().KubeconfigFile(); got != "/tmp/test-kubeconfig" {
		t.Fatalf("expected kubeconfig to be preserved, got %q", got)
	}
}

func TestNewFallsBackToDefaultControllerNamespace(t *testing.T) {
	env := New()

	if got := env.ControllerNamespace(); got != DefaultControllerNamespace {
		t.Fatalf("expected default controller namespace %q, got %q", DefaultControllerNamespace, got)
	}
}

func TestAllocateNamespaceUsesPrefixAndSanitizesName(t *testing.T) {
	env := New(WithNamespacePrefix("fsb-e2e"))

	first := env.AllocateNamespace("Basic Validation")
	second := env.AllocateNamespace("Basic Validation")

	if first == second {
		t.Fatalf("expected allocated namespaces to be unique, both were %q", first)
	}
	if first[:8] != "fsb-e2e-" {
		t.Fatalf("expected namespace %q to use configured prefix", first)
	}
	if second[:8] != "fsb-e2e-" {
		t.Fatalf("expected namespace %q to use configured prefix", second)
	}
	if containsUppercase(first) || containsUppercase(second) {
		t.Fatalf("expected allocated namespaces to be lowercase, got %q and %q", first, second)
	}
}

func TestAllocateNamespaceIsUniqueAcrossSuiteRuns(t *testing.T) {
	first := New(WithNamespacePrefix("fsb-e2e")).AllocateNamespace("port")
	second := New(WithNamespacePrefix("fsb-e2e")).AllocateNamespace("port")
	if first == second {
		t.Fatalf("separate suite runs reused namespace %q", first)
	}
}

func TestRunCleanupsRunsInReverseRegistrationOrder(t *testing.T) {
	env := New()
	var order []string

	env.RegisterCleanup(func(context.Context) error {
		order = append(order, "first")
		return nil
	})
	env.RegisterCleanup(func(context.Context) error {
		order = append(order, "second")
		return nil
	})

	if err := env.RunCleanups(context.Background()); err != nil {
		t.Fatalf("expected cleanups to succeed, got error: %v", err)
	}

	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("expected reverse cleanup order, got %v", order)
	}
}

func TestRunCleanupsReturnsFirstError(t *testing.T) {
	env := New()
	want := errors.New("cleanup failed")

	env.RegisterCleanup(func(context.Context) error {
		return want
	})

	if err := env.RunCleanups(context.Background()); !errors.Is(err, want) {
		t.Fatalf("expected cleanup error %v, got %v", want, err)
	}
}

func TestFastletImageUsesDefaultWhenUnset(t *testing.T) {
	t.Setenv("FAST_SANDBOX_FASTLET_IMAGE", "")
	t.Setenv("FASTLET_IMAGE", "")

	if got := FastletImage(); got != "fast-sandbox/fastlet:dev" {
		t.Fatalf("expected default fastlet image, got %q", got)
	}
}

func TestFastletImagePrefersFastSandboxSpecificEnv(t *testing.T) {
	t.Setenv("FASTLET_IMAGE", "fallback:dev")
	t.Setenv("FAST_SANDBOX_FASTLET_IMAGE", "preferred:dev")

	if got := FastletImage(); got != "preferred:dev" {
		t.Fatalf("expected FAST_SANDBOX_FASTLET_IMAGE to win, got %q", got)
	}
}

func TestSmallSandboxResourceProfileIsComplete(t *testing.T) {
	profile := SmallSandboxResourceProfile()
	if profile.CPU.Sign() <= 0 || profile.Memory.Sign() <= 0 || profile.PIDs <= 0 {
		t.Fatalf("small Sandbox resource profile must be fully positive: %+v", profile)
	}
}

func TestRequireBasicUsesBasicProfile(t *testing.T) {
	original := requireProfile
	t.Cleanup(func() {
		requireProfile = original
	})

	var got e2eenv.Profile
	requireProfile = func(t testing.TB, profile e2eenv.Profile) *e2eenv.Manager {
		got = profile
		return nil
	}

	RequireBasic(t)

	if got != e2eenv.ProfileBasic {
		t.Fatalf("profile = %q, want %q", got, e2eenv.ProfileBasic)
	}
}

func TestRequireGVisorUsesGVisorProfile(t *testing.T) {
	original := requireProfile
	t.Cleanup(func() {
		requireProfile = original
	})

	var got e2eenv.Profile
	requireProfile = func(t testing.TB, profile e2eenv.Profile) *e2eenv.Manager {
		got = profile
		return nil
	}

	RequireGVisor(t)

	if got != e2eenv.ProfileGVisor {
		t.Fatalf("profile = %q, want %q", got, e2eenv.ProfileGVisor)
	}
}

func TestRequireKataProfiles(t *testing.T) {
	tests := []struct {
		name    string
		require func(testing.TB) *e2eenv.Manager
		want    e2eenv.Profile
	}{
		{name: "qemu", require: RequireKataQemu, want: e2eenv.ProfileKataQemu},
		{name: "clh", require: RequireKataClh, want: e2eenv.ProfileKataClh},
		{name: "fc", require: RequireKataFc, want: e2eenv.ProfileKataFc},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := requireProfile
			t.Cleanup(func() {
				requireProfile = original
			})

			var got e2eenv.Profile
			requireProfile = func(t testing.TB, profile e2eenv.Profile) *e2eenv.Manager {
				got = profile
				return nil
			}

			tt.require(t)

			if got != tt.want {
				t.Fatalf("profile = %q, want %q", got, tt.want)
			}
		})
	}
}

func containsUppercase(value string) bool {
	for _, r := range value {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}
