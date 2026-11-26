package fastpath

import (
	"testing"

	"github.com/stretchr/testify/require"

	fastpathv2 "fast-sandbox/api/proto/v2"
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

func TestExtractSensitiveActionInputs(t *testing.T) {
	handlers := []apiv1alpha2.ActionHandler{
		{Name: "egress", TargetHTTPPort: 18080},
		{Name: "runtime-init", Exec: &apiv1alpha2.ExecActionHandler{Command: []string{"/usr/bin/curl", "-H", "@{{.Sensitive.File}}"}}},
	}

	// Exec handler accepts sensitive_input; CRD ActionBinding omits it.
	extracted, err := extractSensitiveActionInputs([]*fastpathv2.ActionBinding{
		{Handler: "egress", Input: `{"v":1}`},
		{Handler: "runtime-init", Input: `{"sandboxId":"sb-1"}`, SensitiveInput: "X-Token: secret\n"},
	}, handlers)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"runtime-init": "X-Token: secret\n"}, extracted)

	// HTTP handler rejects sensitive_input.
	_, err = extractSensitiveActionInputs([]*fastpathv2.ActionBinding{
		{Handler: "egress", Input: `{"v":1}`, SensitiveInput: "X-Token: secret\n"},
	}, handlers)
	require.ErrorContains(t, err, "only supported for exec Action Handlers")
}
