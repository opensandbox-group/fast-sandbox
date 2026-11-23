package fastpath

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	pb "fast-sandbox/api/proto/v2"
	"fast-sandbox/internal/fastletauth"
)

const CallerAudience = "fast-sandbox-fastpath"

type CallerGrant struct {
	Subject    string   `json:"subject"`
	Profile    string   `json:"profile"`
	Namespaces []string `json:"namespaces"`
}

func LoadCallerPolicy(path string) ([]CallerGrant, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var grants []CallerGrant
	if err := decoder.Decode(&grants); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing caller policy data")
	}
	return grants, nil
}

type callerClaimBound struct {
	issued  float64
	expires float64
}

type CallerAuthenticator struct {
	policy     map[string]CallerGrant
	create     func() (*fastletauth.Authenticator, error)
	mu         sync.Mutex
	verifier   *fastletauth.Authenticator
	claims     map[[32]byte]callerClaimBound
	loaded     time.Time
	retry      time.Time
	refreshing bool
	flight     chan struct{}
	now        func() time.Time
}

func NewCallerAuthenticator(issuer string, grants []CallerGrant, client *http.Client) (*CallerAuthenticator, error) {
	if len(grants) == 0 {
		return nil, fmt.Errorf("FastPath caller policy must not be empty")
	}
	policy := make(map[string]CallerGrant)
	subjects := make([]string, 0, len(grants))
	for _, grant := range grants {
		parts := strings.Split(grant.Subject, ":")
		if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || len(validation.IsDNS1123Label(parts[2])) != 0 || len(validation.IsDNS1123Subdomain(parts[3])) != 0 {
			return nil, fmt.Errorf("invalid FastPath caller subject")
		}
		if grant.Profile != "server" && grant.Profile != "gateway" {
			return nil, fmt.Errorf("invalid FastPath caller profile")
		}
		if len(grant.Namespaces) == 0 {
			return nil, fmt.Errorf("caller namespace scope is required")
		}
		for _, ns := range grant.Namespaces {
			if ns != "*" && len(validation.IsDNS1123Label(ns)) != 0 {
				return nil, fmt.Errorf("invalid caller namespace scope")
			}
		}
		if _, duplicate := policy[grant.Subject]; duplicate {
			return nil, fmt.Errorf("duplicate caller subject")
		}
		grant.Namespaces = append([]string(nil), grant.Namespaces...)
		policy[grant.Subject] = grant
		subjects = append(subjects, grant.Subject)
	}
	a := &CallerAuthenticator{policy: policy, claims: make(map[[32]byte]callerClaimBound), now: time.Now}
	a.create = func() (*fastletauth.Authenticator, error) {
		return fastletauth.NewAuthenticator(issuer, CallerAudience, subjects, client, fastletauth.WithSameOriginKeys())
	}
	verifier, err := a.create()
	if err != nil {
		return nil, err
	}
	a.verifier, a.loaded = verifier, a.now()
	return a, nil
}

func (a *CallerAuthenticator) startRefreshLocked() chan struct{} {
	if a.flight != nil {
		return a.flight
	}
	flight := make(chan struct{})
	a.flight = flight
	a.refreshing = true
	go func() {
		next, err := a.create()
		a.mu.Lock()
		defer a.mu.Unlock()
		a.refreshing = false
		a.flight = nil
		a.retry = a.now().Add(30 * time.Second)
		if err == nil {
			a.verifier, a.claims, a.loaded = next, make(map[[32]byte]callerClaimBound), a.now()
		} else {
			klog.ErrorS(err, "FastPath caller key refresh failed")
		}
		close(flight)
	}()
	return flight
}

func (a *CallerAuthenticator) current(ctx context.Context) (*fastletauth.Authenticator, error) {
	a.mu.Lock()
	now := a.now()
	age := now.Sub(a.loaded)
	var flight chan struct{}
	if age >= 5*time.Minute && (a.flight != nil || !now.Before(a.retry)) {
		flight = a.startRefreshLocked()
	}
	if age < 15*time.Minute {
		verifier := a.verifier
		a.mu.Unlock()
		return verifier, nil
	}
	a.mu.Unlock()
	if flight != nil {
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-flight:
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.now().Sub(a.loaded) < 15*time.Minute {
			return a.verifier, nil
		}
	}
	return nil, status.Error(codes.Unavailable, "FastPath verification keys unavailable")
}

func (a *CallerAuthenticator) checkCallerClaims(raw string) ([32]byte, callerClaimBound, bool, error) {
	digest := sha256.Sum256([]byte(raw))
	now := a.now()
	current := float64(now.UnixNano()) / 1e9
	a.mu.Lock()
	bound, cached := a.claims[digest]
	if cached && current >= bound.expires {
		delete(a.claims, digest)
		cached = false
	}
	a.mu.Unlock()
	if cached {
		if bound.issued > current+30 || current >= bound.expires {
			return digest, callerClaimBound{}, true, status.Error(codes.Unauthenticated, "caller token must have a valid lifetime of at most one hour")
		}
		return digest, bound, true, nil
	}
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(raw, claims); err != nil {
		return digest, callerClaimBound{}, false, status.Error(codes.Unauthenticated, "invalid caller token")
	}
	if err := validateCallerClaims(claims, now); err != nil {
		return digest, callerClaimBound{}, false, err
	}
	return digest, callerClaimBound{issued: claims["iat"].(float64), expires: claims["exp"].(float64)}, false, nil
}

func (a *CallerAuthenticator) authenticate(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) > 16384 {
		return "", status.Error(codes.Unauthenticated, "one FastPath bearer credential is required")
	}
	raw := strings.TrimPrefix(values[0], "Bearer ")
	digest, bound, cached, err := a.checkCallerClaims(raw)
	if err != nil {
		return "", err
	}
	verifier, err := a.current(ctx)
	if err != nil {
		return "", err
	}
	subject, err := verifier.Authenticate(ctx, raw)
	switch {
	case errors.Is(err, fastletauth.ErrUnauthorized):
		return "", status.Error(codes.PermissionDenied, "FastPath caller is not allowed")
	case errors.Is(err, fastletauth.ErrUnavailable):
		return "", status.Error(codes.Unavailable, "FastPath caller verification unavailable")
	case err != nil:
		return "", status.Error(codes.Unauthenticated, "invalid caller token")
	}
	if !cached {
		a.mu.Lock()
		if len(a.claims) >= 8192 {
			clear(a.claims)
		}
		a.claims[digest] = bound
		a.mu.Unlock()
	}
	return subject, nil
}

func validateCallerClaims(claims jwt.MapClaims, now time.Time) error {
	issued, iok := claims["iat"].(float64)
	expires, eok := claims["exp"].(float64)
	current := float64(now.UnixNano()) / 1e9
	if !iok || !eok || expires <= issued || expires-issued > 3600 || issued > current+30 || current >= expires {
		return status.Error(codes.Unauthenticated, "caller token must have a valid lifetime of at most one hour")
	}
	audience, err := claims.GetAudience()
	if err != nil || len(audience) != 1 || audience[0] != CallerAudience {
		return status.Error(codes.Unauthenticated, "invalid caller token audience")
	}
	return nil
}

type callerSubjectKey struct{}

func (a *CallerAuthenticator) UnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	subject, err := a.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	method, namespace := callerTarget(req)
	grant := a.policy[subject]
	allowedMethod := method != "" && info.FullMethod == "/fastpath.v2.FastPathService/"+method
	if grant.Profile == "gateway" {
		allowedMethod = allowedMethod && (method == "ResolveEndpoint" || method == "GetSandbox" || method == "ResumeSandbox")
	}
	allowedNamespace := false
	for _, ns := range grant.Namespaces {
		if namespace != "" && (ns == "*" || ns == namespace) {
			allowedNamespace = true
		}
	}
	if !allowedMethod || !allowedNamespace {
		klog.InfoS("FastPath caller denied", "subject", subject, "method", info.FullMethod, "namespace", namespace)
		return nil, status.Error(codes.PermissionDenied, "FastPath RPC or namespace is not allowed")
	}
	return handler(context.WithValue(ctx, callerSubjectKey{}, subject), req)
}

func (a *CallerAuthenticator) StreamInterceptor(_ any, _ grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
	return status.Error(codes.PermissionDenied, "FastPath streaming RPC has no caller policy")
}

func callerTarget(req any) (string, string) {
	switch r := req.(type) {
	case *pb.CreateSandboxRequest:
		return "CreateSandbox", r.GetNamespace()
	case *pb.CreateRootfsSandboxRequest:
		return "CreateRootfsSandbox", r.GetCreate().GetNamespace()
	case *pb.GetSandboxRequest:
		return "GetSandbox", r.GetSandbox().GetNamespacedName().GetNamespace()
	case *pb.DeleteRequest:
		return "DeleteSandbox", r.GetSandbox().GetNamespacedName().GetNamespace()
	case *pb.UpdateSandboxRequest:
		return "UpdateSandbox", r.GetSandbox().GetNamespacedName().GetNamespace()
	case *pb.ListSandboxesRequest:
		return "ListSandboxes", r.GetNamespace()
	case *pb.SandboxDiagnosticsRequest:
		return "GetSandboxDiagnostics", r.GetNamespace()
	case *pb.ResolveEndpointRequest:
		return "ResolveEndpoint", r.GetSandbox().GetNamespacedName().GetNamespace()
	case *pb.GetPoolRequest:
		return "GetPool", r.GetNamespace()
	case *pb.ListPoolsRequest:
		return "ListPools", r.GetNamespace()
	case *pb.CreateSandboxSnapshotRequest:
		return "CreateSandboxSnapshot", r.GetSandbox().GetNamespacedName().GetNamespace()
	case *pb.GetSandboxSnapshotRequest:
		return "GetSandboxSnapshot", r.GetSnapshot().GetNamespace()
	case *pb.DeleteSandboxSnapshotRequest:
		return "DeleteSandboxSnapshot", r.GetSnapshot().GetNamespacedName().GetNamespace()
	case *pb.PauseSandboxRequest:
		return "PauseSandbox", r.GetSandbox().GetNamespacedName().GetNamespace()
	case *pb.ResumeSandboxRequest:
		return "ResumeSandbox", r.GetSandbox().GetNamespacedName().GetNamespace()
	case *pb.InvokeHandlerActionRequest:
		return "InvokeHandlerAction", r.GetSandbox().GetNamespacedName().GetNamespace()
	default:
		return "", ""
	}
}
