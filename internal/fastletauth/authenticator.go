package fastletauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// verdictCacheTTL is how long a validation verdict is reused. A cached
// verdict makes the per-request cost a hash and a map lookup: signature
// verification and subject checks run once per token per interval, not once
// per request. Negative verdicts use a shorter TTL so an operator fixing the
// allow list or RBAC recovers quickly.
const (
	positiveVerdictTTL = time.Minute
	negativeVerdictTTL = 5 * time.Second
	// expiryLeeway tolerates minor clock skew between the issuer and the
	// Fastlet node.
	expiryLeeway = 30 * time.Second
)

// DefaultAllowedSubjects is the subject allow list when the deployment does
// not override it: the platform control-plane ServiceAccounts.
var DefaultAllowedSubjects = []string{
	"system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath",
	"system:serviceaccount:fast-sandbox-system:fast-sandbox-controller",
}

// AuthenticatedJWT is the verified identity of a token.
type AuthenticatedJWT struct {
	Subject string
}

// Authenticator validates Fastlet control tokens locally against the issuer's
// published signing keys and an explicit ServiceAccount allow list. It is
// fail-closed: any signature, expiry, issuer, audience, or subject mismatch
// rejects the request.
type Authenticator struct {
	fetcher  *jwksFetcher
	audience string
	allowed  map[string]struct{}
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cachedVerdict
}

type cachedVerdict struct {
	subject   string
	err       error
	expiresAt time.Time
}

// NewAuthenticator builds an authenticator anchored at issuer, accepting only
// the given ServiceAccount subjects (exact `system:serviceaccount:<namespace>:<name>`
// usernames). An empty allow list rejects every token; httpClient is optional
// and lets tests pin the transport.
func NewAuthenticator(issuer, audience string, allowedSubjects []string, httpClient *http.Client) *Authenticator {
	allowed := make(map[string]struct{}, len(allowedSubjects))
	for _, subject := range allowedSubjects {
		if subject = strings.TrimSpace(subject); subject != "" {
			allowed[subject] = struct{}{}
		}
	}
	return &Authenticator{
		fetcher:  newJWKSFetcher(issuer, httpClient, time.Now),
		audience: audience,
		allowed:  allowed,
		now:      time.Now,
		cache:    map[string]cachedVerdict{},
	}
}

// AllowedSubjects returns the caller subjects accepted by this authenticator.
func (a *Authenticator) AllowedSubjects() []string {
	subjects := make([]string, 0, len(a.allowed))
	for subject := range a.allowed {
		subjects = append(subjects, subject)
	}
	sort.Strings(subjects)
	return subjects
}

// Authenticate verifies the token and returns its ServiceAccount subject.
// The returned error wraps ErrUnauthenticated, ErrUnauthorized, or
// ErrUnavailable so HTTP mapping stays in one place.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	key := base64.RawStdEncoding.EncodeToString(digest[:])
	if subject, err, ok := a.cachedVerdict(key); ok {
		return subject, err
	}
	subject, verdict := a.verify(ctx, token)
	ttl := positiveVerdictTTL
	if verdict != nil {
		ttl = negativeVerdictTTL
	}
	a.storeVerdict(key, subject, verdict, a.now().Add(ttl))
	return subject, verdict
}

func (a *Authenticator) cachedVerdict(key string) (string, error, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	verdict, found := a.cache[key]
	if !found {
		return "", nil, false
	}
	if !a.now().Before(verdict.expiresAt) {
		delete(a.cache, key)
		return "", nil, false
	}
	return verdict.subject, verdict.err, true
}

func (a *Authenticator) storeVerdict(key, subject string, verdict error, expiresAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cache[key] = cachedVerdict{subject: subject, err: verdict, expiresAt: expiresAt}
}

// verify runs the full verification pipeline: parse, signature against the
// issuer key set, and the exp/iss/aud/sub claims.
func (a *Authenticator) verify(ctx context.Context, token string) (string, error) {
	header, payload, signature, err := parseJWT(token)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if header.Algorithm != "RS256" && header.Algorithm != "ES256" {
		return "", fmt.Errorf("%w: unsupported signing algorithm %q", ErrUnauthenticated, header.Algorithm)
	}
	key, err := a.fetcher.keyFor(ctx, header.KeyID)
	if err != nil {
		return "", err
	}
	signingInput := []byte(token[:strings.LastIndex(token, ".")])
	if err := verifySignature(header.Algorithm, key, signingInput, signature); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("%w: token payload is not valid JSON: %v", ErrUnauthenticated, err)
	}
	if claims.ExpiresAt == 0 {
		return "", fmt.Errorf("%w: token has no exp claim; never-expiring tokens are not accepted", ErrUnauthenticated)
	}
	if !a.now().Add(expiryLeeway).Before(time.Unix(claims.ExpiresAt, 0)) {
		return "", fmt.Errorf("%w: token is expired", ErrUnauthenticated)
	}
	if claims.NotBefore != 0 && a.now().Add(expiryLeeway).Before(time.Unix(claims.NotBefore, 0)) {
		return "", fmt.Errorf("%w: token is not valid yet", ErrUnauthenticated)
	}
	if claims.Issuer != a.fetcher.issuer {
		return "", fmt.Errorf("%w: token issuer %q is not the anchored issuer", ErrUnauthenticated, claims.Issuer)
	}
	if !containsAudience(claims.Audiences, a.audience) {
		return "", fmt.Errorf("%w: token audience %v does not include %q", ErrUnauthenticated, claims.Audiences, a.audience)
	}
	if claims.Subject == "" {
		return "", fmt.Errorf("%w: token has no sub claim", ErrUnauthenticated)
	}
	if _, permitted := a.allowed[claims.Subject]; !permitted {
		return "", fmt.Errorf("%w: subject %q is not in the allow list", ErrUnauthorized, claims.Subject)
	}
	return claims.Subject, nil
}

// jwtHeader is the JWT header subset the verifier enforces.
type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

// jwtClaims is the JWT claim subset the verifier enforces. Audiences may be
// a string or a list per the JWT spec; k8s TokenRequest tokens emit a list.
type jwtClaims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audiences []string `json:"aud"`
	ExpiresAt int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	IssuedAt  int64    `json:"iat"`
}

func parseJWT(token string) (jwtHeader, []byte, []byte, error) {
	headerSegment, rest, found := strings.Cut(token, ".")
	if !found {
		return jwtHeader{}, nil, nil, fmt.Errorf("token is not a JWT")
	}
	payloadSegment, signatureSegment, found := strings.Cut(rest, ".")
	if !found || strings.Contains(signatureSegment, ".") {
		return jwtHeader{}, nil, nil, fmt.Errorf("token is not a three-part JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(headerSegment)
	if err != nil {
		return jwtHeader{}, nil, nil, fmt.Errorf("decode token header: %w", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadSegment)
	if err != nil {
		return jwtHeader{}, nil, nil, fmt.Errorf("decode token payload: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureSegment)
	if err != nil {
		return jwtHeader{}, nil, nil, fmt.Errorf("decode token signature: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return jwtHeader{}, nil, nil, fmt.Errorf("parse token header: %w", err)
	}
	if header.KeyID == "" {
		return jwtHeader{}, nil, nil, fmt.Errorf("token header has no kid")
	}
	return header, payload, signature, nil
}

func verifySignature(algorithm string, key cryptoPublicKey, signingInput, signature []byte) error {
	digest := sha256.Sum256(signingInput)
	switch algorithm {
	case "RS256":
		publicKey := key.RSAPublicKey()
		if publicKey == nil {
			return fmt.Errorf("RS256 token presented with a non-RSA signing key")
		}
		return rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature)
	case "ES256":
		publicKey := key.ECDSAPublicKey()
		if publicKey == nil {
			return fmt.Errorf("ES256 token presented with a non-EC signing key")
		}
		if len(signature) != 64 {
			return fmt.Errorf("ES256 signature is not a 64-byte r||s value")
		}
		var r, s big.Int
		r.SetBytes(signature[:32])
		s.SetBytes(signature[32:])
		if !ecdsa.Verify(publicKey, digest[:], &r, &s) {
			return fmt.Errorf("ES256 signature verification failed")
		}
		return nil
	default:
		return fmt.Errorf("unsupported signing algorithm %q", algorithm)
	}
}

func containsAudience(audiences []string, audience string) bool {
	for _, candidate := range audiences {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(audience)) == 1 {
			return true
		}
	}
	return false
}
