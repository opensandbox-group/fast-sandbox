package contract

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

const (
	HeaderFastletPodUID      = "X-Fast-Sandbox-Fastlet-Pod-Uid"
	HeaderAssignmentAttempt  = "X-Fast-Sandbox-Assignment-Attempt"
	HeaderRouteGeneration    = "X-Fast-Sandbox-Route-Generation"
	HeaderForwardedNamespace = "X-Fast-Sandbox-Namespace"
)

type RoutePublication struct {
	Namespace             string
	SandboxUID            string
	FastletPodUID         string
	AssignmentAttempt     int64
	RouteGeneration       int64
	Access                AccessDescriptor
	UpstreamHeadersByPort map[uint32]map[string]string
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

func CloneHeadersByPort(headers map[uint32]map[string]string) map[uint32]map[string]string {
	if headers == nil {
		return nil
	}
	clone := make(map[uint32]map[string]string, len(headers))
	for port, values := range headers {
		clone[port] = cloneStringMap(values)
	}
	return clone
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
