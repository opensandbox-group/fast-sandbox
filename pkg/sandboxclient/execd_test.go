package sandboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type staticResolver struct{ route Route }

func (r staticResolver) Resolve(_ context.Context, _ SandboxRef, port uint32) (Route, error) {
	route := r.route
	route.TargetPort = port
	return route, nil
}

func adapterForServer(t *testing.T, server *httptest.Server) *ExecdAdapter {
	t.Helper()
	endpoint, err := url.Parse(server.URL + "/v1/sandboxes/uid-a/ports/44772")
	require.NoError(t, err)
	return &ExecdAdapter{Resolver: staticResolver{route: Route{
		Endpoint: endpoint, RequiredHeaders: http.Header{"Authorization": []string{"Bearer route-token"}},
	}}, HTTPClient: server.Client()}
}

func TestExecdAdapterStreamsCommandThroughResolvedRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/v1/sandboxes/uid-a/ports/44772/command", request.URL.Path)
		require.Equal(t, "Bearer route-token", request.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		require.Equal(t, "'printf' '%s' 'hello world'", body["command"])
		require.Equal(t, float64(1500), body["timeout"])
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"init\",\"text\":\"cmd-a\"}\n\n")
		_, _ = io.WriteString(writer, "{\"type\":\"stdout\",\"text\":\"hello\\n\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"stderr\",\"text\":\"warn\\n\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
	}))
	defer server.Close()
	adapter := adapterForServer(t, server)
	command, err := ShellJoin([]string{"printf", "%s", "hello world"})
	require.NoError(t, err)
	var streamed bytes.Buffer
	execution, err := adapter.RunCommand(context.Background(), SandboxRef{Name: "sandbox-a"}, RunCommandRequest{
		Command: command, Timeout: 1500 * time.Millisecond,
	}, &ExecutionHandlers{OnStdout: func(message OutputMessage) error {
		streamed.WriteString(message.Text)
		return nil
	}})
	require.NoError(t, err)
	require.Equal(t, "cmd-a", execution.ID)
	require.Equal(t, "hello\n", streamed.String())
	require.Equal(t, "warn\n", execution.Stderr[0].Text)
	require.NotNil(t, execution.ExitCode)
	require.Zero(t, *execution.ExitCode)
}

func TestExecdAdapterFilesUseProtocolEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer route-token", request.Header.Get("Authorization"))
		switch request.Method + " " + request.URL.Path {
		case "GET /v1/sandboxes/uid-a/ports/44772/files/info":
			require.Equal(t, "/tmp/value", request.URL.Query().Get("path"))
			_, _ = io.WriteString(writer, `{"/tmp/value":{"path":"/tmp/value","size":5,"mode":644}}`)
		case "GET /v1/sandboxes/uid-a/ports/44772/files/download":
			_, _ = io.WriteString(writer, "value")
		case "POST /v1/sandboxes/uid-a/ports/44772/files/upload":
			reader, err := request.MultipartReader()
			require.NoError(t, err)
			metadataPart, err := reader.NextPart()
			require.NoError(t, err)
			metadata, err := io.ReadAll(metadataPart)
			require.NoError(t, err)
			require.JSONEq(t, `{"path":"/tmp/value","mode":644}`, string(metadata))
			filePart, err := reader.NextPart()
			require.NoError(t, err)
			require.Equal(t, "value", readString(t, filePart))
		case "POST /v1/sandboxes/uid-a/ports/44772/directories":
			require.JSONEq(t, `{"/tmp/dir":{"mode":755}}`, readString(t, request.Body))
		default:
			http.Error(writer, fmt.Sprintf("unexpected request %s %s", request.Method, request.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()
	adapter := adapterForServer(t, server)
	sandbox := SandboxRef{Name: "sandbox-a"}

	info, err := adapter.Stat(context.Background(), sandbox, "/tmp/value")
	require.NoError(t, err)
	require.Equal(t, int64(5), info.Size)
	var downloaded bytes.Buffer
	written, err := adapter.Download(context.Background(), sandbox, "/tmp/value", &downloaded)
	require.NoError(t, err)
	require.Equal(t, int64(5), written)
	require.NoError(t, adapter.Upload(context.Background(), sandbox, "/tmp/value", strings.NewReader("value"), 0o644))
	require.NoError(t, adapter.MakeDir(context.Background(), sandbox, "/tmp/dir", 0o755))
}

func TestExecdAdapterRejectsEmptySSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
	}))
	defer server.Close()
	_, err := adapterForServer(t, server).RunCommand(context.Background(), SandboxRef{Name: "sandbox-a"}, RunCommandRequest{Command: "true"}, nil)
	require.ErrorContains(t, err, "empty event stream")
}

func readString(t *testing.T, reader io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	return string(data)
}
