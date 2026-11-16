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
	// issuer certified by the injected CA.
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
