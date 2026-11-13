package fastletauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// unknownKeyRefreshInterval throttles JWKS refreshes triggered by unknown
// signing keys, so a flood of forged kid values cannot turn into a request
// storm against the API server.
const unknownKeyRefreshInterval = 30 * time.Second

// maxKeyDocumentBytes bounds the discovery and JWKS response bodies.
const maxKeyDocumentBytes = 1 << 20

// oidcDiscovery is the subset of the OIDC discovery document the verifier
// needs.
type oidcDiscovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// jwksDocument is the JSON Web Key Set served by the issuer.
type jwksDocument struct {
	Keys []jwksKey `json:"keys"`
}

// jwksKey is one JSON Web Key. Only RSA (RS256) and EC P-256 (ES256) signing
// keys are accepted; Kubernetes ServiceAccount signing keys are one of these.
type jwksKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// cryptoPublicKey is a JWT verification key.
type cryptoPublicKey interface {
	RSAPublicKey() *rsa.PublicKey
	ECDSAPublicKey() *ecdsa.PublicKey
}

type rsaKey struct{ key *rsa.PublicKey }

func (k rsaKey) RSAPublicKey() *rsa.PublicKey     { return k.key }
func (k rsaKey) ECDSAPublicKey() *ecdsa.PublicKey { return nil }

type ecdsaKey struct{ key *ecdsa.PublicKey }

func (k ecdsaKey) RSAPublicKey() *rsa.PublicKey     { return nil }
func (k ecdsaKey) ECDSAPublicKey() *ecdsa.PublicKey { return k.key }

func (k jwksKey) publicKey() (cryptoPublicKey, error) {
	switch k.Kty {
	case "RSA":
		if k.Alg != "" && k.Alg != "RS256" {
			return nil, fmt.Errorf("RSA key %q uses unsupported alg %q", k.Kid, k.Alg)
		}
		modulus, err := decodeBase64URLBigInt(k.N)
		if err != nil {
			return nil, fmt.Errorf("decode RSA modulus for key %q: %w", k.Kid, err)
		}
		exponent, err := decodeBase64URLBigInt(k.E)
		if err != nil {
			return nil, fmt.Errorf("decode RSA exponent for key %q: %w", k.Kid, err)
		}
		if !exponent.IsInt64() || exponent.Int64() <= 0 || exponent.Int64() > int64(^uint32(0)>>1) {
			return nil, fmt.Errorf("RSA exponent for key %q is out of range", k.Kid)
		}
		return rsaKey{key: &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}}, nil
	case "EC":
		if k.Alg != "" && k.Alg != "ES256" {
			return nil, fmt.Errorf("EC key %q uses unsupported alg %q", k.Kid, k.Alg)
		}
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("EC key %q uses unsupported curve %q", k.Kid, k.Crv)
		}
		x, err := decodeBase64URLBigInt(k.X)
		if err != nil {
			return nil, fmt.Errorf("decode EC x for key %q: %w", k.Kid, err)
		}
		y, err := decodeBase64URLBigInt(k.Y)
		if err != nil {
			return nil, fmt.Errorf("decode EC y for key %q: %w", k.Kid, err)
		}
		return ecdsaKey{key: &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}}, nil
	default:
		return nil, fmt.Errorf("key %q has unsupported kty %q", k.Kid, k.Kty)
	}
}

func decodeBase64URLBigInt(encoded string) (*big.Int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(raw), nil
}

// keySet is the parsed public key set of one issuer, keyed by kid.
type keySet struct {
	keys map[string]cryptoPublicKey
}

// jwksFetcher discovers and caches the signing keys of one issuer. The issuer
// URL is the trust anchor: it comes from the kubelet-written projected token
// of this Pod (or an explicit override), never from an incoming token, so an
// attacker cannot redirect key material to a host they control.
type jwksFetcher struct {
	issuer string
	client *http.Client
	now    func() time.Time

	mu             sync.Mutex
	keys           *keySet
	lastKeyRefresh time.Time
}

func newJWKSFetcher(issuer string, client *http.Client, now func() time.Time) *jwksFetcher {
	return &jwksFetcher{issuer: issuer, client: client, now: now}
}

// keyFor returns the verification key for kid, refreshing the JWKS once when
// the kid is unknown so API server key rotation is followed without restarts.
func (f *jwksFetcher) keyFor(ctx context.Context, kid string) (cryptoPublicKey, error) {
	f.mu.Lock()
	keys, lastRefresh := f.keys, f.lastKeyRefresh
	f.mu.Unlock()
	if keys != nil {
		if key, found := keys.keys[kid]; found {
			return key, nil
		}
		if !lastRefresh.IsZero() && f.now().Sub(lastRefresh) < unknownKeyRefreshInterval {
			return nil, fmt.Errorf("%w: signing key %q is not in the cached key set", ErrUnauthenticated, kid)
		}
	}
	refreshed, err := f.refresh(ctx)
	if err != nil {
		return nil, err
	}
	if key, found := refreshed.keys[kid]; found {
		return key, nil
	}
	return nil, fmt.Errorf("%w: signing key %q is not served by the issuer", ErrUnauthenticated, kid)
}

// refresh re-fetches the discovery document and JWKS and swaps them in
// atomically. A failed refresh keeps serving the previous keys.
func (f *jwksFetcher) refresh(ctx context.Context) (*keySet, error) {
	f.mu.Lock()
	if f.client == nil {
		f.client = defaultHTTPClient()
	}
	client := f.client
	f.mu.Unlock()

	discovery, err := fetchDiscovery(ctx, client, f.issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: fetch OIDC discovery: %v", ErrUnavailable, err)
	}
	keys, err := fetchJWKS(ctx, client, discovery.JWKSURI)
	if err != nil {
		return nil, fmt.Errorf("%w: fetch JWKS: %v", ErrUnavailable, err)
	}
	f.mu.Lock()
	f.keys, f.lastKeyRefresh = keys, f.now()
	f.mu.Unlock()
	return keys, nil
}

func fetchDiscovery(ctx context.Context, client *http.Client, issuer string) (*oidcDiscovery, error) {
	if issuer == "" {
		return nil, fmt.Errorf("issuer is empty")
	}
	parsed, err := url.Parse(issuer)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, fmt.Errorf("issuer %q is not a valid URL", issuer)
	}
	document := &oidcDiscovery{}
	if err := fetchJSONDocument(ctx, client, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", document); err != nil {
		return nil, err
	}
	if document.Issuer != issuer {
		return nil, fmt.Errorf("discovery issuer %q does not match %q", document.Issuer, issuer)
	}
	if document.JWKSURI == "" {
		return nil, fmt.Errorf("discovery document has no jwks_uri")
	}
	return document, nil
}

func fetchJWKS(ctx context.Context, client *http.Client, jwksURI string) (*keySet, error) {
	document := &jwksDocument{}
	if err := fetchJSONDocument(ctx, client, jwksURI, document); err != nil {
		return nil, err
	}
	keys := make(map[string]cryptoPublicKey, len(document.Keys))
	for _, entry := range document.Keys {
		if entry.Use != "" && entry.Use != "sig" {
			continue
		}
		if entry.Kid == "" {
			continue
		}
		key, err := entry.publicKey()
		if err != nil {
			// A key set may advertise key types this verifier does not use;
			// skip them and fail only when a needed kid is missing.
			continue
		}
		keys[entry.Kid] = key
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS contains no usable signing keys")
	}
	return &keySet{keys: keys}, nil
}

func fetchJSONDocument(ctx context.Context, client *http.Client, url string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned status %d", url, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxKeyDocumentBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxKeyDocumentBytes {
		return fmt.Errorf("GET %s returned an oversized document", url)
	}
	return json.Unmarshal(raw, target)
}

// defaultHTTPClient builds the client used to fetch issuer key material. The
// trust set is the cluster CA bundle injected by the Pool Controller
// (CABundleEnv, base64 PEM) or the Pod's own projected ca.crt, whichever is
// available; a managed-cluster issuer signed by a public CA verifies through
// the system roots fallback.
func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caRaw, ok := injectedCABundle(); ok {
		transport.TLSClientConfig = &tls.Config{RootCAs: caRaw}
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

// injectedCABundle resolves the CA trust set: the injected base64 bundle
// first, the projected service account CA second. It returns ok=false when
// neither is present, leaving the transport on the system roots.
func injectedCABundle() (*x509.CertPool, bool) {
	pool := x509.NewCertPool()
	if encoded := os.Getenv(CABundleEnv); encoded != "" {
		if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded)); err == nil && pool.AppendCertsFromPEM(raw) {
			return pool, true
		}
		klog.ErrorS(nil, "Fastlet auth CA bundle is set but not parseable as base64 PEM; falling back to system roots")
	}
	if raw, err := os.ReadFile(serviceAccountCAFile); err == nil && pool.AppendCertsFromPEM(raw) {
		return pool, true
	}
	return nil, false
}
