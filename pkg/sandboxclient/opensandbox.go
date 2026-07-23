package sandboxclient

import (
	"context"
	"errors"
	"net/http"
	"strings"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

const OpenSandboxExecdPort uint32 = 44772

// OpenSandboxExecd resolves a Fast Sandbox route and hands the resulting
// endpoint plus all route headers to the official OpenSandbox Go Execd SDK.
// Fast Sandbox owns discovery/authentication; the upstream SDK owns the Execd
// command and file protocol.
type OpenSandboxExecd struct {
	Resolver   RouteResolver
	HTTPClient *http.Client
	Port       uint32
}

func (a *OpenSandboxExecd) Client(ctx context.Context, sandbox SandboxRef) (*opensandbox.ExecdClient, Route, error) {
	if a == nil || a.Resolver == nil {
		return nil, Route{}, errors.New("OpenSandbox Execd route resolver is not configured")
	}
	port := a.Port
	if port == 0 {
		port = OpenSandboxExecdPort
	}
	route, err := a.Resolver.Resolve(ctx, sandbox, port)
	if err != nil {
		return nil, Route{}, err
	}
	headers := make(map[string]string, len(route.RequiredHeaders))
	for name, values := range route.RequiredHeaders {
		if len(values) > 0 {
			headers[name] = values[len(values)-1]
		}
	}
	options := []opensandbox.Option{opensandbox.WithHeaders(headers)}
	if a.HTTPClient != nil {
		options = append(options, opensandbox.WithHTTPClient(a.HTTPClient))
	}
	return opensandbox.NewExecdClient(strings.TrimRight(route.Endpoint.String(), "/"), "", options...), route, nil
}

// ShellJoin produces one POSIX shell command without relying on a shell in the
// control plane. OpenSandbox Execd intentionally accepts a command string.
func ShellJoin(arguments []string) (string, error) {
	if len(arguments) == 0 {
		return "", errors.New("at least one command argument is required")
	}
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		if argument == "" {
			quoted[index] = "''"
			continue
		}
		quoted[index] = "'" + strings.ReplaceAll(argument, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " "), nil
}
