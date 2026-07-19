package sandboxclient

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvdAdapterHandsResolvedRouteToUpstreamSDK(t *testing.T) {
	endpoint, err := url.Parse("https://proxy.test/v1/sandboxes/uid-a/ports/49983")
	require.NoError(t, err)
	adapter := &EnvdAdapter{Resolver: staticResolver{route: Route{
		Endpoint: endpoint, RequiredHeaders: http.Header{"Authorization": []string{"Bearer route-token"}},
	}}}

	resolved, err := adapter.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"})
	require.NoError(t, err)
	require.Equal(t, endpoint.String(), resolved.BaseURL)
	require.Equal(t, "Bearer route-token", resolved.Headers.Get("Authorization"))
	resolved.Headers.Set("Authorization", "mutated")
	require.Equal(t, "Bearer route-token", resolved.Route.RequiredHeaders.Get("Authorization"))
}
