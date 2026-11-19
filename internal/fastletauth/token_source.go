package fastletauth

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	authenticationv1 "k8s.io/api/authentication/v1"
)

// ServiceAccountTokenFile is the projected ServiceAccount token the kubelet
// writes and rotates. The Fastlet reads its key-fetch credential from here;
// the Pool Controller derives the projected volume mount from it, so the two
// cannot drift.
const ServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Service account projection locations mounted into every in-cluster Pod by
// default. The namespace file gives the Pod namespace; the projected token's
// `sub` claim gives the ServiceAccount name.
const (
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	serviceAccountCAFile        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

	// DefaultExpirationSeconds asks for a one-hour token. The control plane
	// refreshes it well before expiry, so callers never observe rotation.
	DefaultExpirationSeconds = 3600
	// DefaultRefreshBefore is the remaining lifetime below which the source
	// mints the next token.
	DefaultRefreshBefore = 20 * time.Minute
)

// PodServiceAccountIdentity is the in-pod identity used to mint Fastlet
// tokens: the Pod's namespace and ServiceAccount name.
type PodServiceAccountIdentity struct {
	Namespace      string
	ServiceAccount string
}

// ResolvePodServiceAccountIdentity reads the in-pod ServiceAccount identity:
// namespace from the projected namespace file, name from the projected
// token's `sub` claim (parsed, not verified — the kubelet wrote it for this
// Pod). Env overrides cover processes running outside a Pod.
func ResolvePodServiceAccountIdentity(namespaceEnv, serviceAccountEnv string) (PodServiceAccountIdentity, error) {
	identity := PodServiceAccountIdentity{Namespace: namespaceEnv, ServiceAccount: serviceAccountEnv}
	if identity.Namespace == "" {
		raw, err := os.ReadFile(serviceAccountNamespaceFile)
		if err != nil {
			return PodServiceAccountIdentity{}, fmt.Errorf("read pod namespace: %w", err)
		}
		identity.Namespace = strings.TrimSpace(string(raw))
	}
	if identity.ServiceAccount == "" {
		raw, err := os.ReadFile(ServiceAccountTokenFile)
		if err != nil {
			return PodServiceAccountIdentity{}, fmt.Errorf("read pod service account token: %w", err)
		}
		subject, err := unverifiedJWTStringClaim(strings.TrimSpace(string(raw)), "sub")
		if err != nil {
			return PodServiceAccountIdentity{}, err
		}
		const serviceAccountSubjectPrefix = "system:serviceaccount:"
		subject, found := strings.CutPrefix(subject, serviceAccountSubjectPrefix)
		if !found {
			return PodServiceAccountIdentity{}, fmt.Errorf("pod token subject is not a %s<namespace>:<name> identity", serviceAccountSubjectPrefix)
		}
		namespace, name, found := strings.Cut(subject, ":")
		if !found || namespace == "" || name == "" {
			return PodServiceAccountIdentity{}, fmt.Errorf("pod token subject is not a parseable service account identity")
		}
		identity.Namespace, identity.ServiceAccount = namespace, name
	}
	if identity.Namespace == "" || identity.ServiceAccount == "" {
		return PodServiceAccountIdentity{}, fmt.Errorf("service account identity is incomplete: namespace=%q serviceAccount=%q", identity.Namespace, identity.ServiceAccount)
	}
	return identity, nil
}

// ResolvePodIssuer reads the issuer URL of this Pod's projected ServiceAccount
// token; it anchors Fastlet-side verification, so key material is fetched
// only from this issuer, never from a URL named by an incoming token.
// issuerEnv overrides for processes running outside a Pod.
func ResolvePodIssuer(issuerEnv string) (string, error) {
	if issuerEnv != "" {
		return issuerEnv, nil
	}
	raw, err := os.ReadFile(ServiceAccountTokenFile)
	if err != nil {
		return "", fmt.Errorf("read pod service account token: %w", err)
	}
	return unverifiedJWTStringClaim(strings.TrimSpace(string(raw)), "iss")
}

// ResolveIssuerAnchor reads the issuer and cluster CA bundle the Pool
// Controller injects into Fastlet Pods (which run without
// automountServiceAccountToken, so they cannot derive the anchor themselves).
// Both inputs honor env overrides for non-Pod processes.
func ResolveIssuerAnchor(issuerEnv, caBundleEnv string) (issuer string, caBundle string, err error) {
	issuer, err = ResolvePodIssuer(issuerEnv)
	if err != nil {
		return "", "", err
	}
	if encoded := strings.TrimSpace(caBundleEnv); encoded != "" {
		if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
			return "", "", fmt.Errorf("decode %s override: %w", CABundleEnv, err)
		}
		return issuer, encoded, nil
	}
	caRaw, err := os.ReadFile(serviceAccountCAFile)
	if err != nil {
		return "", "", fmt.Errorf("read pod service account CA bundle: %w", err)
	}
	return issuer, base64.StdEncoding.EncodeToString(caRaw), nil
}

// unverifiedJWTStringClaim decodes a string claim of a JWT payload without
// verifying the signature. ParseUnverified still parses the real wire shape
// (numeric exp, array aud, the kubernetes.io object), so kubelet-projected
// tokens decode cleanly.
func unverifiedJWTStringClaim(token, claim string) (string, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return "", fmt.Errorf("parse pod token: %w", err)
	}
	value, ok := claims[claim].(string)
	if !ok {
		return "", fmt.Errorf("pod token payload has no string %s claim", claim)
	}
	return value, nil
}

// CreateTokenFunc is the narrowed Kubernetes TokenRequest call: create a
// token for the named ServiceAccount in the namespace.
type CreateTokenFunc func(ctx context.Context, namespace, name string, request *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error)

// TokenRequestSource mints audience-bound, expiring ServiceAccount tokens
// through the TokenRequest API and refreshes them before expiry: Kubernetes
// signs and rotates the credential, so the deployment holds no key material.
type TokenRequestSource struct {
	// CreateToken is the TokenRequest API binding, typically
	// authenticationv1client's ServiceAccounts().CreateToken.
	CreateToken       CreateTokenFunc
	Namespace         string
	ServiceAccount    string
	Audience          string
	ExpirationSeconds int64
	RefreshBefore     time.Duration
	// RequestTimeout bounds a single TokenRequest call.
	RequestTimeout time.Duration
	now            func() time.Time

	mu        sync.Mutex
	token     string
	refreshAt time.Time
	// mint is the in-flight mint. Concurrent callers join one flight and
	// await its shared outcome with their own contexts, instead of queueing
	// sequential network calls while mu is held; closing flight.done
	// broadcasts the stored outcome to every joined caller.
	mint *tokenFlight
}

// tokenFlight is one shared mint: the outcome is stored before done closes,
// so every caller that observes the close reads a complete result.
type tokenFlight struct {
	done  chan struct{}
	token string
	err   error
}

// NewTokenRequestSource builds a source with the package defaults applied to
// unset fields.
func NewTokenRequestSource(createToken CreateTokenFunc, identity PodServiceAccountIdentity, audience string) *TokenRequestSource {
	return &TokenRequestSource{
		CreateToken:       createToken,
		Namespace:         identity.Namespace,
		ServiceAccount:    identity.ServiceAccount,
		Audience:          audience,
		ExpirationSeconds: DefaultExpirationSeconds,
		RefreshBefore:     DefaultRefreshBefore,
		RequestTimeout:    5 * time.Second,
		now:               time.Now,
	}
}

// Token returns the current token, minting a replacement through the
// TokenRequest API when none is cached or the cached one is close to expiry.
// Mints are single-flight: one leader performs the API call while followers
// await its shared outcome under their own deadlines, so an N-caller stampede
// on an expired token costs one TokenRequest instead of N sequential network
// calls that per-call timeouts cannot bound.
func (s *TokenRequestSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.token != "" && s.now().Before(s.refreshAt) {
		token := s.token
		s.mu.Unlock()
		return token, nil
	}
	if s.CreateToken == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("%w: token source has no TokenRequest binding", ErrUnavailable)
	}
	if s.mint == nil {
		s.mint = &tokenFlight{done: make(chan struct{})}
		go s.mintToken(s.mint)
	}
	flight := s.mint
	s.mu.Unlock()

	select {
	case <-flight.done:
		return flight.token, flight.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// mintToken performs the TokenRequest outside mu, stores the outcome, and
// broadcasts it to every caller that joined the flight. The request is
// bounded by RequestTimeout alone: a leader whose caller cancels still
// completes the shared mint, because followers are already awaiting its
// outcome.
func (s *TokenRequestSource) mintToken(flight *tokenFlight) {
	requestContext, cancel := context.WithTimeout(context.Background(), s.requestTimeout())
	defer cancel()
	expirationSeconds := s.ExpirationSeconds
	issued, err := s.CreateToken(requestContext, s.Namespace, s.ServiceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{s.audience()},
			ExpirationSeconds: &expirationSeconds,
		},
	})
	switch {
	case err != nil:
		flight.err = fmt.Errorf("%w: request %s/%s token: %w", ErrUnavailable, s.Namespace, s.ServiceAccount, err)
	case issued.Status.Token == "":
		flight.err = fmt.Errorf("%w: TokenRequest returned an empty token", ErrUnavailable)
	default:
		flight.token = issued.Status.Token
		expiresAt := s.now().Add(time.Duration(expirationSeconds) * time.Second)
		if !issued.Status.ExpirationTimestamp.IsZero() {
			expiresAt = issued.Status.ExpirationTimestamp.Time
		}
		// Clamp the refresh margin to half the granted lifetime: a cluster
		// may serve a shorter token than requested, and pinning refreshAt to
		// now would mint one token per call.
		margin := s.refreshBefore()
		if lifetime := expiresAt.Sub(s.now()); margin > lifetime/2 {
			margin = lifetime / 2
		}
		s.mu.Lock()
		s.token = flight.token
		s.refreshAt = expiresAt.Add(-margin)
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.mint == flight {
		s.mint = nil
	}
	s.mu.Unlock()
	close(flight.done)
}

func (s *TokenRequestSource) requestTimeout() time.Duration {
	if s.RequestTimeout > 0 {
		return s.RequestTimeout
	}
	return 5 * time.Second
}

func (s *TokenRequestSource) audience() string {
	if s.Audience != "" {
		return s.Audience
	}
	return Audience
}

func (s *TokenRequestSource) refreshBefore() time.Duration {
	if s.RefreshBefore > 0 {
		return s.RefreshBefore
	}
	return DefaultRefreshBefore
}
