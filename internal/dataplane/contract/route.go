package contract

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

const (
	HeaderRouteCredential    = "X-Fast-Sandbox-Route-Credential"
	HeaderFastletPodUID      = "X-Fast-Sandbox-Fastlet-Pod-Uid"
	HeaderAssignmentAttempt  = "X-Fast-Sandbox-Assignment-Attempt"
	HeaderRouteGeneration    = "X-Fast-Sandbox-Route-Generation"
	HeaderForwardedNamespace = "X-Fast-Sandbox-Namespace"
	// HeaderProxyError is emitted only for platform routing failures that
	// happen before a request reaches the Sandbox application. Proxies remove
	// any application-supplied response value before forwarding.
	HeaderProxyError = "X-Fast-Sandbox-Proxy-Error"
)

const (
	ProxyErrorRouteUnavailable    = "route_unavailable"
	ProxyErrorComponentNotFound   = "component_not_found"
	ProxyErrorComponentNotReady   = "component_not_ready"
	ProxyErrorStaleRoute          = "stale_route"
	ProxyErrorCredentialRejected  = "credential_rejected"
	ProxyErrorUpstreamUnavailable = "upstream_unavailable"
)

type RoutePublication struct {
	Namespace         string
	SandboxUID        string
	FastletPodUID     string
	AssignmentAttempt int64
	RouteGeneration   int64
	Access            AccessDescriptor
	Components        map[string]ComponentRoute
}

type ComponentRoute struct {
	Protocol string `json:"protocol"`
	Port     uint32 `json:"port"`
}

// RecoveryRoutePublisher temporarily fences access without deleting ownership.
// Resume must be explicit; ordinary delayed publications cannot lift the fence.
type RecoveryRoutePublisher interface {
	SuspendRoute(context.Context, RoutePublication) error
	ResumeRoute(context.Context, RoutePublication) error
}

type RoutePublisher interface {
	ApplyRoute(context.Context, RoutePublication) error
	RemoveRoute(context.Context, RoutePublication) error
	ReconcileRoutes(context.Context, []RoutePublication) error
}

func ParseRoutePath(path string) (string, uint32, string, error) {
	const prefix = "/v1/sandboxes/"
	if !strings.HasPrefix(path, prefix) {
		return "", 0, "", errors.New("route path must start with /v1/sandboxes/")
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 || parts[0] == "" || parts[1] != "ports" || parts[2] == "" {
		return "", 0, "", errors.New("route path must be /v1/sandboxes/{uid}/ports/{port}/...")
	}
	uid, err := url.PathUnescape(parts[0])
	if err != nil || uid == "" || strings.Contains(uid, "/") {
		return "", 0, "", errors.New("invalid sandbox UID")
	}
	portValue, err := strconv.ParseUint(parts[2], 10, 16)
	if err != nil || portValue == 0 {
		return "", 0, "", errors.New("target port must be between 1 and 65535")
	}
	suffix := "/"
	if len(parts) == 4 && parts[3] != "" {
		suffix += parts[3]
	}
	return uid, uint32(portValue), suffix, nil
}

func RoutePath(sandboxUID string, targetPort uint32) string {
	return "/v1/sandboxes/" + url.PathEscape(sandboxUID) + "/ports/" + strconv.FormatUint(uint64(targetPort), 10)
}

func ComponentRoutePath(sandboxUID, componentName string) string {
	return "/v2/sandboxes/" + url.PathEscape(sandboxUID) + "/components/" + url.PathEscape(componentName)
}

func ParseComponentRoutePath(path string) (string, string, string, error) {
	const prefix = "/v2/sandboxes/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", "", errors.New("component route path must start with /v2/sandboxes/")
	}
	parts := strings.SplitN(strings.TrimPrefix(path, prefix), "/", 4)
	if len(parts) < 3 || parts[0] == "" || parts[1] != "components" || parts[2] == "" {
		return "", "", "", errors.New("component route path must be /v2/sandboxes/{uid}/components/{name}/...")
	}
	uid, err := url.PathUnescape(parts[0])
	if err != nil || uid == "" || strings.Contains(uid, "/") {
		return "", "", "", errors.New("invalid sandbox UID")
	}
	component, err := url.PathUnescape(parts[2])
	if err != nil || component == "" || strings.Contains(component, "/") {
		return "", "", "", errors.New("invalid component name")
	}
	suffix := "/"
	if len(parts) == 4 && parts[3] != "" {
		suffix += parts[3]
	}
	return uid, component, suffix, nil
}
