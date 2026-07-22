package sandboxclient

import (
	"context"
	"testing"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type fakeEndpointControl struct {
	getRequest     *fastpathv1.GetRequest
	resolveRequest *fastpathv1.ResolveEndpointRequest
}

func (c *fakeEndpointControl) GetSandbox(_ context.Context, request *fastpathv1.GetRequest, _ ...grpc.CallOption) (*fastpathv1.SandboxInfo, error) {
	c.getRequest = request
	return &fastpathv1.SandboxInfo{SandboxUid: "uid-a", SandboxName: request.SandboxName}, nil
}

func (c *fakeEndpointControl) ResolveEndpoint(_ context.Context, request *fastpathv1.ResolveEndpointRequest, _ ...grpc.CallOption) (*fastpathv1.ResolveEndpointResponse, error) {
	c.resolveRequest = request
	return &fastpathv1.ResolveEndpointResponse{
		SandboxUid: request.SandboxUid, TargetPort: request.TargetPort,
		ProxyEndpoint:   "http://sandbox-proxy.svc/v1/sandboxes/uid-a/ports/44772",
		RequiredHeaders: map[string]string{"Authorization": "Bearer route-token"}, RouteGeneration: 3,
	}, nil
}

func TestEndpointResolverPreservesRoutePathWhenAuthorityIsOverridden(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: control, DefaultNamespace: "tenant-a", ProxyBaseURL: "http://127.0.0.1:18080/proxy"}

	route, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, ExecdPort)
	require.NoError(t, err)
	require.Equal(t, "tenant-a", control.getRequest.Namespace)
	require.Equal(t, "uid-a", control.resolveRequest.SandboxUid)
	require.Equal(t, "http://127.0.0.1:18080/proxy/v1/sandboxes/uid-a/ports/44772", route.Endpoint.String())
	require.Equal(t, "Bearer route-token", route.RequiredHeaders.Get("Authorization"))

	requestURL, err := route.RequestURL("/command", nil)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:18080/proxy/v1/sandboxes/uid-a/ports/44772/command", requestURL.String())
}

func TestEndpointResolverRejectsMismatchedRouteIdentity(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: mismatchedEndpointControl{control}}
	_, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, ExecdPort)
	require.ErrorContains(t, err, "different Sandbox")
}

type mismatchedEndpointControl struct{ *fakeEndpointControl }

func (c mismatchedEndpointControl) ResolveEndpoint(ctx context.Context, request *fastpathv1.ResolveEndpointRequest, options ...grpc.CallOption) (*fastpathv1.ResolveEndpointResponse, error) {
	response, err := c.fakeEndpointControl.ResolveEndpoint(ctx, request, options...)
	response.TargetPort++
	return response, err
}
