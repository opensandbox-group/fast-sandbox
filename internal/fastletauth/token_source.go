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

// Service account projection locations mounted into every in-cluster Pod by
// default. The namespace file gives the Pod namespace; the projected token's
// `sub` claim gives the ServiceAccount name.
const (
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	serviceAccountTokenFile     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
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
// the namespace from the projected namespace file and the ServiceAccount name
// from the projected token's `sub` claim. Environment overrides win when set,
// which covers processes running outside a Pod (integration tooling). The
// projected token is only parsed, never verified: the file is written by the
// kubelet for this Pod.
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
		raw, err := os.ReadFile(serviceAccountTokenFile)
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
// token. It anchors the Fastlet-side JWT verifier: key material is fetched
// only from this issuer, never from a URL named by an incoming token. An
// explicit issuerEnv override covers processes running outside a Pod. The
// projected token is only parsed, never verified: the file is written by the
// kubelet for this Pod.
func ResolvePodIssuer(issuerEnv string) (string, error) {
	if issuerEnv != "" {
		return issuerEnv, nil
	}
	raw, err := os.ReadFile(serviceAccountTokenFile)
	if err != nil {
		return "", fmt.Errorf("read pod service account token: %w", err)
	}
	return unverifiedJWTStringClaim(strings.TrimSpace(string(raw)), "iss")
}

// ResolveIssuerAnchor reads the issuer anchor the Pool Controller injects
// into Fastlet Pods: this Pod's own issuer URL and the cluster CA bundle that
// authenticates TLS to it. Fastlet Pods run with
// automountServiceAccountToken forced off, so they cannot derive the anchor
// themselves; the control plane, which holds its projected ServiceAccount
// token, ships it with the Pod template. Both inputs honor environment
// overrides (FASTLET_AUTH_ISSUER, FASTLET_AUTH_CA_BUNDLE, base64-encoded
// PEM) for processes running outside a Pod; the override format matches the
// injected format exactly.
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
// through the Kubernetes TokenRequest API and refreshes them before expiry.
// Kubernetes signs and later revokes the credential: the deployment holds no
// key material, and rotation is driven by token lifetime instead of operator
// action.
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
func (s *TokenRequestSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.refreshAt) {
		return s.token, nil
	}
	if s.CreateToken == nil {
		return "", fmt.Errorf("%w: token source has no TokenRequest binding", ErrUnavailable)
	}
	expirationSeconds := s.ExpirationSeconds
	requestContext, cancel := context.WithTimeout(ctx, s.requestTimeout())
	defer cancel()
	issued, err := s.CreateToken(requestContext, s.Namespace, s.ServiceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{s.audience()},
			ExpirationSeconds: &expirationSeconds,
		},
	})
	if err != nil {
		return "", fmt.Errorf("%w: request %s/%s token: %v", ErrUnavailable, s.Namespace, s.ServiceAccount, err)
	}
	if issued.Status.Token == "" {
		return "", fmt.Errorf("%w: TokenRequest returned an empty token", ErrUnavailable)
	}
	expiresAt := s.now().Add(time.Duration(expirationSeconds) * time.Second)
	if !issued.Status.ExpirationTimestamp.IsZero() {
		expiresAt = issued.Status.ExpirationTimestamp.Time
	}
	s.token = issued.Status.Token
	// Clamp the refresh margin to half the granted lifetime: a cluster may
	// serve a shorter token than requested (--service-account-max-token-
	// expiration), and pinning refreshAt to now would otherwise mint one
	// token per control call.
	margin := s.refreshBefore()
	if lifetime := expiresAt.Sub(s.now()); margin > lifetime/2 {
		margin = lifetime / 2
	}
	s.refreshAt = expiresAt.Add(-margin)
	return s.token, nil
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
