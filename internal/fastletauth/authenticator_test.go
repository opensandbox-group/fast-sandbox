package fastletauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/fastletauth/fastletauthtest"
)

func newTestAuthenticator(t *testing.T, issuer *fastletauthtest.Issuer, subjects ...string) *Authenticator {
	t.Helper()
	return newTestAuthenticatorWithInterval(t, issuer, defaultRefreshInterval, subjects...)
}

func newTestAuthenticatorWithInterval(t *testing.T, issuer *fastletauthtest.Issuer, refreshInterval time.Duration, subjects ...string) *Authenticator {
	t.Helper()
	if len(subjects) == 0 {
		subjects = []string{"system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath"}
	}
	// The harness ignores the header, but the credential must resolve or
	// construction fails closed, matching production.
	t.Setenv(KeyFetchTokenEnv, "test-key-fetch-credential")
	authenticator, err := NewAuthenticator(issuer.URL(), Audience, subjects, nil, WithRefreshInterval(refreshInterval))
	require.NoError(t, err)
	return authenticator
}

func TestSameOriginKeysRejectsCrossOriginBeforeFetch(t *testing.T) {
	var fetched atomic.Bool
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched.Store(true)
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer keys.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "http://" + r.Host, "jwks_uri": keys.URL})
	}))
	defer issuer.Close()
	_, err := NewAuthenticator(issuer.URL, Audience, nil, issuer.Client(), WithSameOriginKeys())
	require.ErrorContains(t, err, "trusted issuer origin")
	require.False(t, fetched.Load())
	require.True(t, sameOriginURL("https://kubernetes.default.svc", "HTTPS://KUBERNETES.DEFAULT.SVC:443/openid/v1/jwks"))
	require.True(t, sameOriginURL("http://issuer.test:80", "http://issuer.test/keys"))
	require.False(t, sameOriginURL("https://kubernetes.default.svc", "https://kubernetes.default.svc:8443/openid/v1/jwks"))
}

func TestAuthenticateAcceptsRS256Token(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	subject, err := authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{}))
	require.NoError(t, err)
	require.Equal(t, "system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath", subject)
}

func TestAuthenticateAcceptsES256Token(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	subject, err := authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{Kid: fastletauthtest.DefaultECKeyID, Algorithm: "ES256"}))
	require.NoError(t, err)
	require.Equal(t, "system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath", subject)
}

func TestAuthenticateAcceptsScalarAudienceClaim(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	token := issuer.Mint(fastletauthtest.MintOptions{AudienceScalar: true})
	subject, err := authenticator.Authenticate(t.Context(), token)
	require.NoError(t, err, "RFC 7519 allows a scalar aud claim; the library must accept it")
	require.Equal(t, "system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath", subject)
}

func TestAuthenticateFollowsKeyRotation(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticatorWithInterval(t, issuer, time.Millisecond)
	rotatedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	const rotatedKid = "rotated-key"
	// Mint with an unpublished key: verification must fail closed.
	_, err = authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{Kid: rotatedKid, Signer: rotatedKey}))
	require.Error(t, err)
	// Publish the rotated key: the next attempt uses a distinct token (so
	// the negative verdict does not mask it) whose unknown kid triggers the
	// rate-limited JWKS refresh, which now serves the key.
	issuer.PublishKey(rotatedKid, rotatedKey)
	subject, err := authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{
		Kid: rotatedKid, Signer: rotatedKey, ExpiresAt: time.Now().Add(3601 * time.Second).Unix(),
	}))
	require.NoError(t, err)
	require.Equal(t, "system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath", subject)
}

func TestAuthenticateRejectsInvalidTokens(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	now := time.Now().Unix()
	cases := []struct {
		name  string
		token string
		class error
	}{
		{"empty", "", ErrUnauthenticated},
		{"not a jwt", "garbage", ErrUnauthenticated},
		{"two segments", "aa.bb", ErrUnauthenticated},
		{"too many segments", "aa.bb.cc.dd", ErrUnauthenticated},
		{"wrong audience", issuer.Mint(fastletauthtest.MintOptions{Audience: "other-consumer"}), ErrUnauthenticated},
		{"wrong issuer", issuer.Mint(fastletauthtest.MintOptions{Issuer: "https://other-issuer.example"}), ErrUnauthenticated},
		{"expired", issuer.Mint(fastletauthtest.MintOptions{ExpiresAt: now - 3600}), ErrUnauthenticated},
		{"not yet valid", issuer.Mint(fastletauthtest.MintOptions{NotBefore: now + 3600}), ErrUnauthenticated},
		{"bad signature", issuer.Mint(fastletauthtest.MintOptions{Signer: otherKey}), ErrUnauthenticated},
		{"unsupported algorithm", issuer.Mint(fastletauthtest.MintOptions{Algorithm: "HS256"}), ErrUnauthenticated},
		{"no exp claim", issuer.Mint(fastletauthtest.MintOptions{OmitExp: true}), ErrUnauthenticated},
		{"malformed segments", issuer.Mint(fastletauthtest.MintOptions{Segments: []string{"a", "b"}}), ErrUnauthenticated},
		{"not base64 payload", issuer.Mint(fastletauthtest.MintOptions{Segments: []string{"a", "!!not-base64!!", "c"}}), ErrUnauthenticated},
		// A kid that remains unpublished even after refresh is definitive:
		// the issuer does not serve it, so the token cannot be authentic.
		// It is classified as unauthenticated (cached 401) rather than a
		// retryable 503 that would hold refresh limiter budget.
		{"unknown kid", issuer.Mint(fastletauthtest.MintOptions{Kid: "missing", Signer: otherKey}), ErrUnauthenticated},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := authenticator.Authenticate(t.Context(), testCase.token)
			require.ErrorIs(t, err, testCase.class)
		})
	}
}

func TestAuthenticateRejectsNonAllowListedSubject(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer, "system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath")
	token := issuer.Mint(fastletauthtest.MintOptions{Subject: "system:serviceaccount:attacker:default"})
	_, err := authenticator.Authenticate(t.Context(), token)
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestAuthenticateCachesPositiveVerdictWithinTokenLifetime(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	token := issuer.Mint(fastletauthtest.MintOptions{})
	_, err := authenticator.Authenticate(t.Context(), token)
	require.NoError(t, err)
	// Shrink the allow list behind the cache's back: the cached verdict must
	// keep the caller authenticated until the verdict expires.
	authenticator.allowed = map[string]struct{}{}
	_, err = authenticator.Authenticate(t.Context(), token)
	require.NoError(t, err)
	authenticator.now = func() time.Time { return time.Now().Add(positiveVerdictTTL + time.Second) }
	_, err = authenticator.Authenticate(t.Context(), token)
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestAuthenticateClampsVerdictToTokenExpiry(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	// The token is accepted (remaining 60s > 30s leeway) but its verdict must
	// be clamped to expiry-minus-leeway (30s), not the full minute.
	token := issuer.Mint(fastletauthtest.MintOptions{ExpiresAt: time.Now().Add(60 * time.Second).Unix()})
	_, err := authenticator.Authenticate(t.Context(), token)
	require.NoError(t, err)
	authenticator.mu.Lock()
	var remaining time.Duration
	for _, cached := range authenticator.cache {
		remaining = cached.expiresAt.Sub(time.Now())
	}
	authenticator.mu.Unlock()
	require.Greater(t, remaining, time.Duration(0))
	require.LessOrEqual(t, remaining, 30*time.Second, "the verdict must not outlive the token's acceptance horizon")
	// Once the clamped verdict expires the token is re-verified (it is still
	// inside its real lifetime), so revoking the subject now takes effect on
	// the next request instead of waiting out the full verdict TTL.
	authenticator.now = func() time.Time { return time.Now().Add(31 * time.Second) }
	authenticator.allowed = map[string]struct{}{}
	_, err = authenticator.Authenticate(t.Context(), token)
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestAuthenticateBoundsVerdictCache(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	for i := range maxCachedVerdicts + 16 {
		authenticator.storeVerdict(sha256SumForTest(i), "", ErrUnauthenticated, time.Now().Add(time.Minute))
	}
	authenticator.mu.Lock()
	size := len(authenticator.cache)
	authenticator.mu.Unlock()
	require.LessOrEqual(t, size, maxCachedVerdicts, "the verdict cache must stay bounded under a flood of distinct tokens")
	// A full cache evicts to admit the incoming verdict: shedding the
	// newcomer would let a distinct-token flood monopolize every slot and
	// strip legitimate tokens of positive caching entirely.
	authenticator.storeVerdict(sha256SumForTest(maxCachedVerdicts+16), "", ErrUnauthenticated, time.Now().Add(time.Minute))
	authenticator.mu.Lock()
	_, stored := authenticator.cache[sha256SumForTest(maxCachedVerdicts+16)]
	authenticator.mu.Unlock()
	require.True(t, stored, "a full cache must evict an entry rather than reject the incoming verdict")
}

// TestAuthenticateRateLimitsUnknownKeyRefreshes proves the refresh limiter:
// repeated unknown-kid requests must wait for the rate limit between JWKS
// fetches instead of issuing one fetch per request. Each attempt presents a
// fresh token so the negative-verdict cache cannot mask the limiter.
func TestAuthenticateRateLimitsUnknownKeyRefreshes(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	const interval = 200 * time.Millisecond
	authenticator := newTestAuthenticatorWithInterval(t, issuer, interval)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	start := time.Now()
	const attempts = 4
	for i := range attempts {
		token := issuer.Mint(fastletauthtest.MintOptions{Kid: "missing", Signer: otherKey, ExpiresAt: time.Now().Add(3600*time.Second + time.Duration(i)*time.Second).Unix()})
		_, err := authenticator.Authenticate(t.Context(), token)
		require.ErrorIs(t, err, ErrUnauthenticated)
	}
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, interval*time.Duration(attempts-1),
		"unknown-kid refreshes must be rate limited between fetches")
	require.Less(t, issuer.Fetches(), int64(attempts*2),
		"fetches must be bounded below one discovery+JWKS pair per request")
}

// TestAuthenticateClassifiesLimiterExhaustionAsUnavailable pins the
// classification contract: an unknown kid whose refresh actually ran and
// still missed is a definitive forgery (cached 401), but a request aborted
// inside the rate limiter — no fetch ran, so the kid truth is unknown — must
// stay a retryable 503, or a legitimately rotated token would be rejected as
// a forgery during the refresh window.
func TestAuthenticateClassifiesLimiterExhaustionAsUnavailable(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	// An interval longer than refreshWaitMax makes the second unknown-kid
	// request abort inside the limiter before any fetch runs.
	authenticator := newTestAuthenticatorWithInterval(t, issuer, refreshWaitMax+time.Second)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	_, err = authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{Kid: "missing", Signer: otherKey}))
	require.ErrorIs(t, err, ErrUnauthenticated, "an absent kid after a refresh that ran is definitive")
	// Distinct token (different hash), so the negative verdict cannot mask
	// the limiter outcome.
	_, err = authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{
		Kid: "missing", Signer: otherKey, ExpiresAt: time.Now().Add(7200 * time.Second).Unix(),
	}))
	require.ErrorIs(t, err, ErrUnavailable, "limiter exhaustion aborts before any fetch: the outcome must stay retryable")
}

func TestAuthenticateRejectsFutureIssuedAt(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	token := issuer.Mint(fastletauthtest.MintOptions{IssuedAt: time.Now().Add(10 * time.Minute).Unix()})
	_, err := authenticator.Authenticate(t.Context(), token)
	require.ErrorIs(t, err, ErrUnauthenticated, "a future-dated iat must be rejected: issued-at validation is enabled")
}

func TestAuthenticateFailsUnavailableIssuer(t *testing.T) {
	// An unreachable issuer is a validation-infrastructure failure, not a
	// caller rejection: construction (which fetches discovery) fails with
	// the unavailable class.
	t.Setenv(KeyFetchTokenEnv, "test-key-fetch-credential")
	authenticator, err := NewAuthenticator("http://127.0.0.1:1", Audience, DefaultAllowedSubjects, nil)
	require.Error(t, err)
	require.Nil(t, authenticator)
}

func TestAuthenticateDoesNotCacheUnavailableVerdicts(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	rotatedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	// Kill the issuer so the unknown-kid refresh fails, then present a token
	// signed by an unpublished key.
	issuer.Close()
	token := issuer.Mint(fastletauthtest.MintOptions{Kid: "late-key", Signer: rotatedKey})
	_, err = authenticator.Authenticate(t.Context(), token)
	require.ErrorIs(t, err, ErrUnavailable)
	authenticator.mu.Lock()
	size := len(authenticator.cache)
	authenticator.mu.Unlock()
	require.Equal(t, 0, size, "an unavailable verdict must not be cached: it is a validator property, not a token property")
}

func TestAllowedSubjectsIsSortedDeterministically(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer,
		"system:serviceaccount:ns:b", "system:serviceaccount:ns:a", "system:serviceaccount:ns:c")
	require.Equal(t, []string{
		"system:serviceaccount:ns:a", "system:serviceaccount:ns:b", "system:serviceaccount:ns:c",
	}, authenticator.AllowedSubjects())
}

func TestAuthenticateAcceptsECKeyFromJWKS(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	// Replace the published EC key with a fresh one to prove the JWKS is the
	// authority, not the harness defaults.
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	issuer.PublishKey(fastletauthtest.DefaultECKeyID, ecKey)
	authenticator := newTestAuthenticator(t, issuer)
	_, err = authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{Kid: fastletauthtest.DefaultECKeyID, Algorithm: "ES256", Signer: ecKey}))
	require.NoError(t, err)
}

func sha256SumForTest(i int) string {
	digest := sha256.Sum256([]byte{byte(i)})
	return base64.RawStdEncoding.EncodeToString(digest[:])
}
