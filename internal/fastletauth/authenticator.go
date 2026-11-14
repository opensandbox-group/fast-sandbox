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
	"k8s.io/klog/v2"
)

const (
	// positiveVerdictTTL caps verdict reuse; per-request cost is then a hash
	// and a map lookup. Clamped to the token's remaining lifetime.
	positiveVerdictTTL = time.Minute
	// negativeVerdictTTL keeps operator fixes (allow list, RBAC) recovering
	// quickly.
	negativeVerdictTTL = 5 * time.Second
	// expiryLeeway tolerates issuer/Fastlet clock skew.
	expiryLeeway = 30 * time.Second
	// maxCachedVerdicts bounds the cache against distinct-token floods on
	// the unauthenticated control port; dropping a verdict only costs one
	// re-verification.
	maxCachedVerdicts = 8192
	// defaultRefreshInterval rate-limits unknown-KID JWKS refreshes so
	// forged kid floods cannot storm the API server's OIDC endpoints.
	defaultRefreshInterval = 30 * time.Second
	// refreshWaitMax fails a request fast instead of waiting out a
	// rate-limited refresh.
	refreshWaitMax = 2 * time.Second
)

// DefaultAllowedSubjects is the allow list when the deployment does not
// override it: the platform control-plane ServiceAccounts.
var DefaultAllowedSubjects = []string{
	"system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath",
	"system:serviceaccount:fast-sandbox-system:fast-sandbox-controller",
}

// Authenticator verifies control tokens against the issuer's published keys
// and a ServiceAccount allow list, fail-closed.
type Authenticator struct {
	parser   *jwt.Parser
	keyfunc  jwt.Keyfunc
	issuer   string
	audience string
	allowed  map[string]struct{}
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cachedVerdict

	// refreshMu guards lastRefreshFailure: jwkset folds "refresh failed" and
	// "kid not served" into one error, so a recent refresh failure means the
	// keyfunc outcome is unknown and must be a retryable 503, not a cached
	// rejection.
	refreshMu          sync.Mutex
	lastRefreshFailure time.Time
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
	authenticator := &Authenticator{
		issuer:   issuer,
		audience: audience,
		allowed:  allowed,
		now:      time.Now,
		cache:    map[string]cachedVerdict{},
	}
	discovery, err := fetchDiscovery(context.Background(), httpClient, issuer)
	if err != nil {
		return nil, err
	}
	// No RefreshInterval: no background goroutine — keys refresh only on an
	// unknown signing key, rate-limited below.
	storage, err := jwkset.NewStorageFromHTTP(discovery.JWKSURI, jwkset.HTTPClientStorageOptions{
		Ctx:    context.Background(),
		Client: httpClient,
		// jwkset folds "refresh failed" and "kid not served" into one
		// ErrKeyNotFound; record failures so the keyfunc wrapper can tell
		// them apart.
		RefreshErrorHandler: func(ctx context.Context, err error) {
			authenticator.refreshMu.Lock()
			authenticator.lastRefreshFailure = authenticator.now()
			authenticator.refreshMu.Unlock()
			klog.ErrorS(err, "Fastlet auth JWKS refresh failed", "issuer", issuer)
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
	// Classify retrieval failures: after a recent refresh failure the kid
	// truth is unknown (retryable 503); otherwise the issuer served its set
	// and the kid is definitively absent (401).
	keyfuncFn := func(token *jwt.Token) (any, error) {
		key, err := keyFunc.Keyfunc(token)
		if err != nil {
			authenticator.refreshMu.Lock()
			failedRecently := !authenticator.lastRefreshFailure.IsZero() &&
				authenticator.now().Sub(authenticator.lastRefreshFailure) < defaultRefreshInterval
			authenticator.refreshMu.Unlock()
			if failedRecently {
				return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
			}
			return nil, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
		}
		return key, nil
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(), // reject never-expiring tokens
		jwt.WithLeeway(expiryLeeway),
	)
	authenticator.parser = parser
	authenticator.keyfunc = keyfuncFn
	return authenticator, nil
}

// Issuer returns the issuer URL verification is anchored at.
func (a *Authenticator) Issuer() string { return a.issuer }

// AllowedSubjects returns the accepted caller subjects.
func (a *Authenticator) AllowedSubjects() []string {
	subjects := make([]string, 0, len(a.allowed))
	for subject := range a.allowed {
		subjects = append(subjects, subject)
	}
	sort.Strings(subjects)
	return subjects
}

// Authenticate verifies the token and returns its ServiceAccount subject.
// The error wraps ErrUnauthenticated, ErrUnauthorized, or ErrUnavailable.
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
		// Unavailability is a validator property, not a token property:
		// caching it would pin a 503 for the token.
		return subject, verdict
	}
	ttl := negativeVerdictTTL
	if verdict == nil {
		ttl = positiveVerdictTTL
		// Clamp to the token's acceptance horizon so near-expiry tokens do
		// not outlive themselves.
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
	// Sweep only when full: the O(n) pass under a.mu exists to reclaim space,
	// expired entries never need revisiting otherwise.
	if len(a.cache) >= maxCachedVerdicts {
		for cachedKey, cached := range a.cache {
			if !now.Before(cached.expiresAt) {
				delete(a.cache, cachedKey)
			}
		}
	}
	if len(a.cache) >= maxCachedVerdicts {
		// Still full of live verdicts: drop this one; its next request
		// simply re-verifies.
		return
	}
	a.cache[key] = cachedVerdict{subject: subject, err: verdict, expiresAt: expiresAt}
}

// verify parses and validates the token (pinned algorithms, iss/aud/exp/nbf,
// key from the issuer set) and applies the subject allow list. It returns the
// token's expiry for verdict clamping.
func (a *Authenticator) verify(ctx context.Context, token string) (string, time.Time, error) {
	parsed, err := a.parser.Parse(token, a.keyfunc)
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

// classifyParseError maps jwt validation failures onto the caller-facing
// error classes: caller faults (malformed, bad signature, failed claims, kid
// absent even after refresh) are unauthenticated; key retrieval failures
// (unreachable issuer, exhausted limiter) may belong to a legitimate caller
// and surface as retryable 503s instead.
func (a *Authenticator) classifyParseError(err error) error {
	// The keyfunc wrapper pre-classifies retrieval outcomes; honor that.
	if errors.Is(err, ErrUnavailable) {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if errors.Is(err, ErrUnauthenticated) {
		return fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if errors.Is(err, jwkset.ErrKeyNotFound) {
		return fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
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
