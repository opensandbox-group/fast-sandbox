package cmd

import (
	"bytes"
	"context"
	"testing"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/stretchr/testify/require"
)

func TestSandboxDiagnosticsFetchAndTextOutput(t *testing.T) {
	mock := &MockClient{DiagnosticsFunc: func(_ context.Context, request *fastpathv1.SandboxDiagnosticsRequest) (*fastpathv1.SandboxDiagnosticsResponse, error) {
		require.Equal(t, "sandbox-a", request.SandboxName)
		require.Equal(t, "namespace-a", request.Namespace)
		require.EqualValues(t, 7, request.Limit)
		return &fastpathv1.SandboxDiagnosticsResponse{
			Sandbox:         &fastpathv1.SandboxInfo{SandboxName: "sandbox-a", SandboxUid: "uid-a", RuntimeState: "Ready", FastletPod: "fastlet-a"},
			AssignmentState: "synchronized", RuntimeInstanceId: "runtime-a", AssignmentAttempt: 2, FastletReachable: true,
			Events: []*fastpathv1.SandboxDiagnosticEvent{{TimestampUnixNano: 1, Level: "info", Source: "runtime", Phase: "running", Message: "ready"}},
		}, nil
	}}
	response, err := fetchSandboxDiagnostics(context.Background(), mock, "sandbox-a", "namespace-a", 7)
	require.NoError(t, err)
	var output bytes.Buffer
	require.NoError(t, printSandboxDiagnostics(&output, response, "text"))
	require.Contains(t, output.String(), "Fastlet diagnostics: reachable")
	require.Contains(t, output.String(), "runtime")
	require.Contains(t, output.String(), "ready")
}

func TestSandboxDiagnosticsPrintsUnavailableWithoutFailing(t *testing.T) {
	response := &fastpathv1.SandboxDiagnosticsResponse{AssignmentState: "unassigned", FastletError: "no assignment"}
	var output bytes.Buffer
	require.NoError(t, printSandboxDiagnostics(&output, response, "text"))
	require.Contains(t, output.String(), "Fastlet diagnostics: unavailable")
	require.Contains(t, output.String(), "no assignment")
}
