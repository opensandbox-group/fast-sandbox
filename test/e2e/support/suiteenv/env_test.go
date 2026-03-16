package suiteenv

import (
	"context"
	"errors"
	"testing"
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

func containsUppercase(value string) bool {
	for _, r := range value {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}
