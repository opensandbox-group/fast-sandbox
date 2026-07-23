package sandboxclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

type staticResolver struct {
	route Route
	err   error
}

func (r staticResolver) Resolve(_ context.Context, _ SandboxRef, port uint32) (Route, error) {
	result := r.route
	result.TargetPort = port
	return result, r.err
}

func openSandboxAdapterForServer(t *testing.T, server *httptest.Server) *OpenSandboxExecd {
	t.Helper()
	endpoint, err := url.Parse(server.URL + "/v1/sandboxes/uid-a/ports/44772")
	require.NoError(t, err)
	return &OpenSandboxExecd{Resolver: staticResolver{route: Route{
		SandboxUID: "uid-a", Endpoint: endpoint,
		RequiredHeaders: http.Header{"Authorization": []string{"Bearer route-token"}},
	}}}
}

func TestOpenSandboxExecdUsesOfficialSDKAndForwardsRouteHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/v1/sandboxes/uid-a/ports/44772/command", request.URL.Path)
		require.Equal(t, "Bearer route-token", request.Header.Get("Authorization"))
		require.Contains(t, request.Header.Get("User-Agent"), "OpenSandbox-Go-SDK")
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"type\":\"stdout\",\"text\":\"hello\"}\n\n")
		fmt.Fprint(writer, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
	}))
	defer server.Close()
	client, route, err := openSandboxAdapterForServer(t, server).Client(context.Background(), SandboxRef{Name: "sandbox-a"})
	require.NoError(t, err)
	require.Equal(t, OpenSandboxExecdPort, route.TargetPort)
	var events []opensandbox.StreamEvent
	require.NoError(t, client.RunCommand(context.Background(), opensandbox.RunCommandRequest{Command: "printf hello"}, func(event opensandbox.StreamEvent) error {
		events = append(events, event)
		return nil
	}))
	require.Len(t, events, 2)
}

func TestOpenSandboxExecdFileCallsUseOfficialSDK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer route-token", request.Header.Get("Authorization"))
		switch request.URL.Path {
		case "/v1/sandboxes/uid-a/ports/44772/files/info":
			fmt.Fprint(writer, `{`+`"/tmp/value":{"path":"/tmp/value","size":5,"mode":644}`+`}`)
		case "/v1/sandboxes/uid-a/ports/44772/files/download":
			fmt.Fprint(writer, "value")
		case "/v1/sandboxes/uid-a/ports/44772/files/upload":
			require.NoError(t, request.ParseMultipartForm(1<<20))
			file, _, err := request.FormFile("file")
			require.NoError(t, err)
			defer file.Close()
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, _, err := openSandboxAdapterForServer(t, server).Client(context.Background(), SandboxRef{Name: "sandbox-a"})
	require.NoError(t, err)
	info, err := client.GetFileInfo(context.Background(), "/tmp/value")
	require.NoError(t, err)
	require.Equal(t, int64(5), info["/tmp/value"].Size)
	download, err := client.DownloadFile(context.Background(), "/tmp/value", "")
	require.NoError(t, err)
	defer download.Close()
	require.NoError(t, client.UploadFile(context.Background(), strings.NewReader("value"), opensandbox.UploadFileOptions{
		FileName: "value", Metadata: opensandbox.FileMetadata{Path: "/tmp/value", Mode: 644},
	}))
}

func TestShellJoinQuotesArguments(t *testing.T) {
	command, err := ShellJoin([]string{"printf", "%s", "hello world", "it's"})
	require.NoError(t, err)
	require.Equal(t, `'printf' '%s' 'hello world' 'it'"'"'s'`, command)
}
