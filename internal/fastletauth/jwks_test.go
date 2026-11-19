package fastletauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// TestInjectedCABundleExtendsSystemRoots pins the compatibility contract: an
// injected cluster CA must extend the system trust roots, never replace them.
// A Kubernetes whose ServiceAccount issuer is served with a publicly trusted
// certificate is a supported deployment and must keep verifying alongside the
// in-cluster issuer.
func TestInjectedCABundleExtendsSystemRoots(t *testing.T) {
	ca := newTestCA(t, "fastletauth-test-ca")
	t.Setenv(CABundleEnv, base64.StdEncoding.EncodeToString(ca.pem))

	pool, found, err := injectedCABundle()
	require.NoError(t, err)
	require.True(t, found)

	// The injected CA itself must verify an issuer it certified...
	serverCertificate := ca.serverCertificate(t, "issuer.fastletauth.test")
	_, verifyErr := serverCertificate.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "issuer.fastletauth.test"})
	require.NoError(t, verifyErr, "an issuer certified by the injected cluster CA must verify")

	// ...and the pool must equal the system roots plus the bundle, not a
	// replacement pool: an issuer with a system-root-signed certificate
	// keeps verifying too.
	expected, err := x509.SystemCertPool()
	require.NoError(t, err)
	require.True(t, expected.AppendCertsFromPEM(ca.pem))
	require.True(t, pool.Equal(expected),
		"the trust set must be the system roots extended with the injected bundle, not a replacement")

	// End to end: the client built from this bundle fetches from an HTTPS
	// issuer certified by the injected CA. The fetch carries the key-fetch
	// credential; the loopback harness ignores the header but the credential
	// must resolve, matching the authenticated production path.
	t.Setenv(KeyFetchTokenEnv, "test-key-fetch-credential")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverCertificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	client, err := defaultHTTPClient()
	require.NoError(t, err)
	response, err := client.Get(server.URL)
	require.NoError(t, err, "an issuer certified by the injected CA must be reachable")
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
}

// testCA is a self-signed certificate authority for trust-anchor tests.
type testCA struct {
	pem         []byte
	certificate *x509.Certificate
	key         *rsa.PrivateKey
}

func newTestCA(t *testing.T, commonName string) *testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testCA{pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certificate: certificate, key: key}
}

// serverCertificate returns a server certificate for dnsName signed by the CA.
func (c *testCA) serverCertificate(t *testing.T, dnsName string) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, c.certificate, &key.PublicKey, c.key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	)
	require.NoError(t, err)
	certificate.Leaf = leaf
	return certificate
}

// TestDefaultHTTPClientRefusesCrossHostRedirect pins the redirect boundary
// for the credential: authedTransport injects the Authorization header inside
// RoundTrip, below the client's redirect handling, so without an explicit
// guard every hop would re-receive the header — silently undoing the standard
// library's deliberate cross-host Authorization stripping. The issuer is the
// trust anchor; the client must refuse to leave its host and hand the
// redirect response to the fetcher, which fails closed on any non-200.
func TestDefaultHTTPClientRefusesCrossHostRedirect(t *testing.T) {
	var forwarded []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded = append(forwarded, "seen")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(target.Close)

	var anchorCredentials []string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		anchorCredentials = append(anchorCredentials, request.Header.Get("Authorization"))
		http.Redirect(w, request, target.URL+"/openid/v1/jwks", http.StatusFound)
	}))
	t.Cleanup(source.Close)

	t.Setenv(KeyFetchTokenEnv, "live-service-account-token")
	client, err := defaultHTTPClient()
	require.NoError(t, err)

	response, err := client.Get(source.URL + "/.well-known/openid-configuration")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusFound, response.StatusCode,
		"a cross-host redirect must not be followed: the non-200 reaches the fetcher and fails it closed")
	require.Equal(t, []string{BearerPrefix + "live-service-account-token"}, anchorCredentials,
		"the anchor host itself receives the credential")
	require.Empty(t, forwarded,
		"the live ServiceAccount credential must never reach a redirect target on another host")
}

func TestNewAuthenticatorRejectsPlaintextIssuerOutsideLoopback(t *testing.T) {
	authenticator, err := NewAuthenticator("http://issuer.example.com", Audience, DefaultAllowedSubjects, nil)
	require.Error(t, err, "a plaintext issuer outside loopback would let an on-path attacker serve forged keys")
	require.Nil(t, authenticator)
}

// TestKeyFetchCredentialPrefersEnvOverride pins the override order: an env
// credential serves host-process runs without a token file, and the file is
// never consulted when the override resolves.
func TestKeyFetchCredentialPrefersEnvOverride(t *testing.T) {
	t.Setenv(KeyFetchTokenEnv, "  env-token  ")
	previous := readKeyFetchTokenFile
	t.Cleanup(func() { readKeyFetchTokenFile = previous })
	readKeyFetchTokenFile = func() (string, error) {
		return "", errors.New("the mounted file must not be consulted when the override is set")
	}
	token, err := keyFetchCredential()
	require.NoError(t, err)
	require.Equal(t, "env-token", token, "the override must be trimmed, not echoed verbatim")
}

// TestKeyFetchCredentialFailsClosed pins the credential posture: a missing or
// empty mounted token fails the resolution with a directed error, so a fetch
// never degrades into a silent anonymous request.
func TestKeyFetchCredentialFailsClosed(t *testing.T) {
	t.Setenv(KeyFetchTokenEnv, "")
	previous := readKeyFetchTokenFile
	t.Cleanup(func() { readKeyFetchTokenFile = previous })

	readKeyFetchTokenFile = func() (string, error) {
		return "", errors.New("open /var/run/secrets/kubernetes.io/serviceaccount/token: no such file or directory")
	}
	_, err := keyFetchCredential()
	require.ErrorContains(t, err, "no key-fetch credential",
		"the error must direct the operator at the override and the mount")

	readKeyFetchTokenFile = func() (string, error) { return "\n", nil }
	_, err = keyFetchCredential()
	require.ErrorContains(t, err, "is empty", "a whitespace-only token file is no credential")
}

// TestAuthedTransportAttachesRotatingCredential pins the authenticated key
// fetch end to end: every request carries the credential resolved at request
// time, so kubelet's projected-token rotation is followed without restarts,
// and losing the credential fails the request closed before it is sent.
func TestAuthedTransportAttachesRotatingCredential(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen = append(seen, request.Header.Get("Authorization"))
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: authedTransport{base: http.DefaultTransport}}
	t.Setenv(KeyFetchTokenEnv, "token-one")
	response, err := client.Get(server.URL)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, []string{"Bearer token-one"}, seen)

	// A rotated credential applies to the next fetch: the resolution happens
	// inside RoundTrip, never cached on the transport.
	t.Setenv(KeyFetchTokenEnv, "token-two")
	response, err = client.Get(server.URL)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, []string{"Bearer token-one", "Bearer token-two"}, seen)

	// Losing the credential fails the fetch closed; no request reaches the
	// issuer anonymously.
	t.Setenv(KeyFetchTokenEnv, "")
	previous := readKeyFetchTokenFile
	t.Cleanup(func() { readKeyFetchTokenFile = previous })
	readKeyFetchTokenFile = func() (string, error) {
		return "", errors.New("simulated missing token file")
	}
	_, err = client.Get(server.URL)
	require.Error(t, err, "a Fastlet without its key-fetch credential must fail closed")
	require.Len(t, seen, 2, "the failed fetch must not reach the issuer")
}

// TestNewAuthenticatorFailsClosedWithoutKeyFetchCredential pins the startup
// contract: construction fetches discovery through the authenticated client,
// so a Fastlet without its credential fails startup with a directed error
// instead of falling back to an anonymous fetch.
func TestNewAuthenticatorFailsClosedWithoutKeyFetchCredential(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	t.Setenv(KeyFetchTokenEnv, "")
	previous := readKeyFetchTokenFile
	t.Cleanup(func() { readKeyFetchTokenFile = previous })
	readKeyFetchTokenFile = func() (string, error) {
		return "", errors.New("simulated missing token file")
	}
	authenticator, err := NewAuthenticator(issuer.URL(), Audience, DefaultAllowedSubjects, nil)
	require.Error(t, err, "issuer key access is authenticated; an anonymous fallback would betray the design")
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
