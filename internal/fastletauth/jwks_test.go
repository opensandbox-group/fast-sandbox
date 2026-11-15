package fastletauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/fastletauth/fastletauthtest"
)

func TestDefaultHTTPClientFailsClosedOnMalformedCABundle(t *testing.T) {
	t.Setenv(CABundleEnv, "definitely-not-base64!!!")
	_, err := defaultHTTPClient()
	require.Error(t, err, "a set-but-malformed CA bundle must fail closed: it is the trust anchor")

	t.Setenv(CABundleEnv, base64.StdEncoding.EncodeToString([]byte("not a pem bundle")))
	_, err = defaultHTTPClient()
	require.Error(t, err, "valid base64 carrying no PEM block is equally fatal")

	// Without an override the projected ca.crt path (or system roots outside
	// a Pod) applies and the client builds.
	t.Setenv(CABundleEnv, "")
	client, err := defaultHTTPClient()
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestNewAuthenticatorFailsOnMalformedCABundle(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	t.Setenv(CABundleEnv, "!!!")
	authenticator, err := NewAuthenticator(issuer.URL(), Audience, DefaultAllowedSubjects, nil)
	require.Error(t, err, "a corrupt injected anchor must fail startup, matching the controller-side contract")
	require.Nil(t, authenticator)
}

func TestNewAuthenticatorRejectsPlaintextIssuerOutsideLoopback(t *testing.T) {
	authenticator, err := NewAuthenticator("http://issuer.example.com", Audience, DefaultAllowedSubjects, nil)
	require.Error(t, err, "a plaintext issuer outside loopback would let an on-path attacker serve forged keys")
	require.Nil(t, authenticator)
}

// TestFetchDiscoveryValidatesDiscoveryServedJWKSURI pins the trust-anchor
// boundary on the discovery output: an https issuer advertising a plaintext
// off-host jwks_uri must not have its signing keys fetched over http.
func TestFetchDiscoveryValidatesDiscoveryServedJWKSURI(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		document := map[string]any{
			"issuer":   server.URL,
			"jwks_uri": "http://attacker.example.com/jwks",
		}
		require.NoError(t, json.NewEncoder(w).Encode(document))
	}))
	t.Cleanup(server.Close)
	_, err := fetchDiscovery(context.Background(), server.Client(), server.URL)
	// The discovery document itself is served from loopback, so the failure
	// must come from the jwks_uri it advertises, not the issuer URL.
	require.ErrorContains(t, err, "jwks_uri")
	require.ErrorContains(t, err, "must use https outside loopback")
}
