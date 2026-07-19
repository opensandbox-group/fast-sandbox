package sandboxclient

import (
	"context"
	"errors"
	"net/http"
)

const EnvdPort uint32 = 49983

// EnvdEndpoint is the hand-off contract for an E2B SDK/Connect client. Fast
// Sandbox resolves and authenticates the network route but deliberately does
// not redefine envd's Process or Filesystem protobuf services.
type EnvdEndpoint struct {
	BaseURL string
	Headers http.Header
	Route   Route
}

type EnvdAdapter struct {
	Resolver RouteResolver
	Port     uint32
}

func (a *EnvdAdapter) Resolve(ctx context.Context, sandbox SandboxRef) (EnvdEndpoint, error) {
	if a == nil || a.Resolver == nil {
		return EnvdEndpoint{}, errors.New("Envd adapter route resolver is not configured")
	}
	port := a.Port
	if port == 0 {
		port = EnvdPort
	}
	route, err := a.Resolver.Resolve(ctx, sandbox, port)
	if err != nil {
		return EnvdEndpoint{}, err
	}
	return EnvdEndpoint{BaseURL: route.Endpoint.String(), Headers: route.RequiredHeaders.Clone(), Route: route}, nil
}
