package fastletauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

const (
	// positiveVerdictTTL is how long a successful validation verdict is
	// reused, clamped to the token's remaining lifetime (see Authenticate).
	// A cached verdict makes the per-request cost a hash and a map lookup:
	// signature verification and subject checks run once per token per
	// interval, not once per request.
	positiveVerdictTTL = time.Minute
	// negativeVerdictTTL is how long a rejection verdict is reused, so an
	// operator fixing the allow list or RBAC recovers quickly.
	negativeVerdictTTL = 5 * time.Second
	// expiryLeeway tolerates minor clock skew between the issuer and the
	// Fastlet node.
	expiryLeeway = 30 * time.Second
	// maxCachedVerdicts bounds the verdict cache. The middleware sits on the
	// unauthenticated control port, so a flood of distinct tokens must not
	// grow the cache without limit; dropping a verdict only costs the next
	// request one re-verification.
	maxCachedVerdicts = 8192
	// defaultRefreshInterval is the unknown-KID refresh rate limit: JWKS
	// refreshes are triggered by unknown signing keys and rate-limited so a
	// flood of forged kid values cannot become a request storm against the
	// API server's OIDC endpoints.
	defaultRefreshInterval = 30 * time.Second
	// refreshWaitMax bounds how long a request waits for a rate-limited
	// refresh; past it the request fails fast with a retryable error.
	refreshWaitMax = 2 * time.Second
)

// DefaultAllowedSubjects is the subject allow list when the deployment does
// not override it: the platform control-plane ServiceAccounts.
var DefaultAllowedSubjects = []string{
	"system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath",
	"system:serviceaccount:fast-sandbox-system:fast-sandbox-controller",
}

// Authenticator validates Fastlet control tokens locally against the issuer's
// published signing keys and an explicit ServiceAccount allow list. It is
// fail-closed: any signature, expiry, issuer, audience, or subject mismatch
// rejects the request.
type Authenticator struct {
	parser   *jwt.Parser
	keyfunc  keyfunc.Keyfunc
	issuer   string
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

type authenticatorOptions struct {
	refreshInterval time.Duration
}

// AuthenticatorOption customizes the authenticator at construction.
type AuthenticatorOption func(*authenticatorOptions)

// WithRefreshInterval overrides the unknown-KID JWKS refresh rate limit. It
// exists for tests; production uses the default interval.
func WithRefreshInterval(d time.Duration) AuthenticatorOption {
	return func(o *authenticatorOptions) { o.refreshInterval = d }
}

// NewAuthenticator builds an authenticator anchored at issuer, accepting only
// the given ServiceAccount subjects (exact `system:serviceaccount:<namespace>:<name>`
// usernames; an empty allow list rejects every token). The issuer's OIDC
// discovery document is fetched once here and its JWKS is served through a
// rate-limited, self-refreshing key set. httpClient is optional and lets
// tests pin the transport.
func NewAuthenticator(issuer, audience string, allowedSubjects []string, httpClient *http.Client, opts ...AuthenticatorOption) (*Authenticator, error) {
	options := authenticatorOptions{refreshInterval: defaultRefreshInterval}
	for _, opt := range opts {
		opt(&options)
	}
	allowed := make(map[string]struct{}, len(allowedSubjects))
	for _, subject := range allowedSubjects {
		if subject = strings.TrimSpace(subject); subject != "" {
			allowed[subject] = struct{}{}
		}
	}
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	discovery, err := fetchDiscovery(context.Background(), httpClient, issuer)
	if err != nil {
		return nil, err
	}
	// RefreshInterval stays unset on purpose: no background refresh
	// goroutine. Keys refresh only when an unknown signing key shows up,
	// rate-limited by the limiter below.
	storage, err := jwkset.NewStorageFromHTTP(discovery.JWKSURI, jwkset.HTTPClientStorageOptions{
		Ctx:    context.Background(),
		Client: httpClient,
		RefreshErrorHandler: func(ctx context.Context, err error) {
			// Only effectual with RefreshInterval; kept for symmetry and
			// future use.
			_ = ctx
			_ = err
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create issuer key set for %q: %w", issuer, err)
	}
	jwks, err := jwkset.NewHTTPClient(jwkset.HTTPClientOptions{
		HTTPURLs:          map[string]jwkset.Storage{discovery.JWKSURI: storage},
		RefreshUnknownKID: rate.NewLimiter(rate.Every(options.refreshInterval), 1),
		RateLimitWaitMax:  refreshWaitMax,
	})
	if err != nil {
		return nil, fmt.Errorf("create issuer key client for %q: %w", issuer, err)
	}
	keyFunc, err := keyfunc.New(keyfunc.Options{Ctx: context.Background(), Storage: jwks})
	if err != nil {
		return nil, fmt.Errorf("create issuer keyfunc for %q: %w", issuer, err)
	}
	parser := jwt.NewParser(
		// Pin the algorithms: a token naming any other algorithm is rejected
		// before any key material is considered.
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		// Never-expiring tokens are not accepted: an absent exp claim is a
		// rejection, not a pass.
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(expiryLeeway),
	)
	return &Authenticator{
		parser:   parser,
		keyfunc:  keyFunc,
		issuer:   issuer,
		audience: audience,
		allowed:  allowed,
		now:      time.Now,
		cache:    map[string]cachedVerdict{},
	}, nil
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
	subject, expiresAt, verdict := a.verify(ctx, token)
	if verdict != nil && errors.Is(verdict, ErrUnavailable) {
		// Unavailability is a property of the validator, not of the token:
		// never cache it, or one failed fetch pins a 503 for this token and
		// undermines the documented retry contract.
		return subject, verdict
	}
	ttl := negativeVerdictTTL
	if verdict == nil {
		ttl = positiveVerdictTTL
		// Clamp the verdict to the token's acceptance horizon: a token near
		// expiry must not stay accepted past its own lifetime, and revoking
		// a subject must take effect when the verdict expires.
		if remaining := expiresAt.Add(-expiryLeeway).Sub(a.now()); remaining < ttl {
			ttl = remaining
		}
	}
	if ttl > 0 {
		a.storeVerdict(key, subject, verdict, a.now().Add(ttl))
	}
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
	now := a.now()
	// Sweep expired entries: lookups alone never revisit them, so distinct
	// tokens would otherwise accumulate forever.
	for cachedKey, cached := range a.cache {
		if !now.Before(cached.expiresAt) {
			delete(a.cache, cachedKey)
		}
	}
	if len(a.cache) >= maxCachedVerdicts {
		// The cache is full of live verdicts: drop this one rather than
		// evicting live entries. The next request for the token simply
		// re-verifies.
		return
	}
	a.cache[key] = cachedVerdict{subject: subject, err: verdict, expiresAt: expiresAt}
}

// verify runs the full verification pipeline: library-side parsing with
// pinned algorithms and iss/aud/exp/nbf validation against the issuer key
// set, then the subject allow list. It returns the token's expiry so the
// caller can clamp verdict caching to the token's acceptance horizon.
func (a *Authenticator) verify(ctx context.Context, token string) (string, time.Time, error) {
	parsed, err := a.parser.Parse(token, a.keyfunc.Keyfunc)
	if err != nil {
		return "", time.Time{}, a.classifyParseError(err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return "", time.Time{}, fmt.Errorf("%w: token claims are not a map", ErrUnauthenticated)
	}
	subject, ok := claims["sub"].(string)
	if !ok || subject == "" {
		return "", time.Time{}, fmt.Errorf("%w: token has no string sub claim", ErrUnauthenticated)
	}
	if _, permitted := a.allowed[subject]; !permitted {
		return "", time.Time{}, fmt.Errorf("%w: subject %q is not in the allow list", ErrUnauthorized, subject)
	}
	var expiresAt time.Time
	if raw, ok := claims["exp"].(float64); ok {
		expiresAt = time.Unix(int64(raw), 0)
	}
	return subject, expiresAt, nil
}

// classifyParseError maps a jwt validation failure onto the caller-facing
// error classes. Malformed tokens, bad signatures, and failed claim
// validation are caller faults (unauthenticated). Key retrieval failures —
// an unreachable issuer, an exhausted refresh rate limit — are validator
// faults (unavailable): the request may be legitimate, so they must surface
// as retryable 503s, never as rejections.
func (a *Authenticator) classifyParseError(err error) error {
	unauthenticatedSentinels := []error{
		jwt.ErrTokenMalformed,
		jwt.ErrTokenSignatureInvalid,
		jwt.ErrTokenExpired,
		jwt.ErrTokenNotValidYet,
		jwt.ErrTokenUsedBeforeIssued,
		jwt.ErrTokenInvalidClaims,
		jwt.ErrTokenInvalidAudience,
		jwt.ErrTokenInvalidIssuer,
	}
	for _, sentinel := range unauthenticatedSentinels {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%w: %v", ErrUnauthenticated, err)
		}
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}
