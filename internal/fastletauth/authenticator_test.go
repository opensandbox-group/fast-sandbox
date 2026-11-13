package fastletauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/fastletauth/fastletauthtest"
)

func newTestAuthenticator(t *testing.T, issuer *fastletauthtest.Issuer, subjects ...string) *Authenticator {
	t.Helper()
	if len(subjects) == 0 {
		subjects = []string{"system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath"}
	}
	return NewAuthenticator(issuer.URL(), Audience, subjects, nil)
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

func TestAuthenticateFollowsKeyRotation(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	rotatedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	const rotatedKid = "rotated-key"
	// Mint with an unpublished key: verification must fail closed.
	_, err = authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{Kid: rotatedKid, Signer: rotatedKey}))
	require.ErrorIs(t, err, ErrUnauthenticated)
	// Publish the rotated key: past the unknown-key refresh throttle, the
	// verifier refetches the JWKS and accepts the token signed by the new key.
	issuer.PublishKey(rotatedKid, rotatedKey)
	later := time.Now().Add(unknownKeyRefreshInterval + negativeVerdictTTL + time.Second)
	authenticator.now = func() time.Time { return later }
	authenticator.fetcher.now = func() time.Time { return later }
	subject, err := authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{Kid: rotatedKid, Signer: rotatedKey}))
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
	}{
		{"empty", ""},
		{"not a jwt", "garbage"},
		{"two segments", "aa.bb"},
		{"too many segments", "aa.bb.cc.dd"},
		{"wrong audience", issuer.Mint(fastletauthtest.MintOptions{Audience: "other-consumer"})},
		{"wrong issuer", issuer.Mint(fastletauthtest.MintOptions{Issuer: "https://other-issuer.example"})},
		{"expired", issuer.Mint(fastletauthtest.MintOptions{ExpiresAt: now - 3600})},
		{"not yet valid", issuer.Mint(fastletauthtest.MintOptions{NotBefore: now + 3600})},
		{"bad signature", issuer.Mint(fastletauthtest.MintOptions{Signer: otherKey})},
		{"unknown kid", issuer.Mint(fastletauthtest.MintOptions{Kid: "missing", Signer: otherKey})},
		{"unsupported algorithm", issuer.Mint(fastletauthtest.MintOptions{Algorithm: "HS256"})},
		{"no exp claim", mintWithoutClaims(t, map[string]any{
			"iss": issuer.URL(), "sub": "system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath",
			"aud": []string{Audience},
		})},
		{"malformed segments", issuer.Mint(fastletauthtest.MintOptions{Segments: []string{"a", "b"}})},
		{"not base64 payload", issuer.Mint(fastletauthtest.MintOptions{Segments: []string{"a", "!!not-base64!!", "c"}})},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := authenticator.Authenticate(t.Context(), testCase.token)
			require.ErrorIs(t, err, ErrUnauthenticated)
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

// mintWithoutClaims signs a token whose payload is the raw map, so tests can
// omit claims such as exp that Mint always fills in.
func mintWithoutClaims(t *testing.T, payload map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signingInput := encode(map[string]any{"alg": "RS256", "kid": "raw", "typ": "JWT"}) + "." + encode(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestAuthenticateCachesPositiveVerdict(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	token := issuer.Mint(fastletauthtest.MintOptions{})
	_, err := authenticator.Authenticate(t.Context(), token)
	require.NoError(t, err)
	// Shrink the allow list behind the cache's back: the cached verdict must
	// keep the caller authenticated until the verdict TTL elapses.
	authenticator.allowed = map[string]struct{}{}
	_, err = authenticator.Authenticate(t.Context(), token)
	require.NoError(t, err)
	authenticator.now = func() time.Time { return time.Now().Add(positiveVerdictTTL + time.Second) }
	_, err = authenticator.Authenticate(t.Context(), token)
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestAuthenticateThrottlesUnknownKeyRefreshes(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	token := issuer.Mint(fastletauthtest.MintOptions{Kid: "missing", Signer: otherKey})
	_, err = authenticator.Authenticate(t.Context(), token)
	require.ErrorIs(t, err, ErrUnauthenticated)
	fetchesAfterRefresh := issuer.Fetches()
	// A burst of unknown-kid tokens must not turn into a request storm.
	for range 5 {
		_, err = authenticator.Authenticate(t.Context(), token)
		require.ErrorIs(t, err, ErrUnauthenticated)
	}
	require.Equal(t, fetchesAfterRefresh, issuer.Fetches())
	// Past the throttle window the verifier tries the issuer again. The
	// negative verdict cache and the fetcher share the clock, so both move.
	later := time.Now().Add(unknownKeyRefreshInterval + negativeVerdictTTL + time.Second)
	authenticator.now = func() time.Time { return later }
	authenticator.fetcher.now = func() time.Time { return later }
	_, err = authenticator.Authenticate(t.Context(), token)
	require.ErrorIs(t, err, ErrUnauthenticated)
	require.Greater(t, issuer.Fetches(), fetchesAfterRefresh)
}

func TestAuthenticateFailsUnavailableIssuer(t *testing.T) {
	// An unreachable issuer is a validation-infrastructure failure, not a
	// caller rejection: the middleware maps it to 503, the authenticator to
	// ErrUnavailable. The token itself parses, so failure happens at the
	// key fetch.
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := NewAuthenticator("http://127.0.0.1:1", Audience, DefaultAllowedSubjects, nil)
	_, err := authenticator.Authenticate(t.Context(), issuer.Mint(fastletauthtest.MintOptions{}))
	require.ErrorIs(t, err, ErrUnavailable)
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
