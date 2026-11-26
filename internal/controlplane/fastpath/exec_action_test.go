package fastpath

import (
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func TestValidateExecActionBindingsImmutable(t *testing.T) {
	handlers := []apiv1alpha2.ActionHandler{
		{Name: "egress", TargetHTTPPort: 18080},
		{Name: "runtime-init", Exec: &apiv1alpha2.ExecActionHandler{Command: []string{"/usr/bin/curl"}}},
	}
	existing := []apiv1alpha2.ActionBinding{
		{Handler: "egress", Input: `{"v":1}`},
		{Handler: "runtime-init", Input: `{"sandboxId":"sb-1"}`},
	}

	// Updating only the HTTP binding is allowed.
	require.NoError(t, validateExecActionBindingsImmutable(existing, []apiv1alpha2.ActionBinding{
		{Handler: "egress", Input: `{"v":2}`},
		{Handler: "runtime-init", Input: `{"sandboxId":"sb-1"}`},
	}, handlers))

	// Modifying an exec binding input is rejected.
	require.ErrorContains(t, validateExecActionBindingsImmutable(existing, []apiv1alpha2.ActionBinding{
		{Handler: "egress", Input: `{"v":1}`},
		{Handler: "runtime-init", Input: `{"sandboxId":"sb-2"}`},
	}, handlers), "immutable")

	// Removing an exec binding is rejected.
	require.ErrorContains(t, validateExecActionBindingsImmutable(existing, []apiv1alpha2.ActionBinding{
		{Handler: "egress", Input: `{"v":1}`},
	}, handlers), "cannot be removed")

	// Adding an exec binding after creation is rejected.
	require.ErrorContains(t, validateExecActionBindingsImmutable(
		[]apiv1alpha2.ActionBinding{{Handler: "egress", Input: `{"v":1}`}},
		existing,
		handlers,
	), "cannot be added")
}
