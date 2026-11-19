package fastletauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// maxKeyDocumentBytes bounds the discovery and JWKS response bodies.
const maxKeyDocumentBytes = 1 << 20

// maxKeyFetchRedirects caps redirect chains at the standard library default.
const maxKeyFetchRedirects = 10

// keyFetchCredential returns the bearer token for issuer key fetches: the
// KeyFetchTokenEnv override, else the mounted projected ServiceAccount token.
// Resolved per request so kubelet rotation is followed; a missing credential
// fails closed with a directed error.
func keyFetchCredential() (string, error) {
	if override := strings.TrimSpace(os.Getenv(KeyFetchTokenEnv)); override != "" {
		return override, nil
	}
	token, err := readKeyFetchTokenFile()
	switch {
	case err != nil:
		return "", fmt.Errorf("no key-fetch credential: set %s or mount the ServiceAccount token file: %w", KeyFetchTokenEnv, err)
	case strings.TrimSpace(token) == "":
		return "", fmt.Errorf("no key-fetch credential: %s is empty; set %s or restore the mounted ServiceAccount token", ServiceAccountTokenFile, KeyFetchTokenEnv)
	}
	return strings.TrimSpace(token), nil
}

// readKeyFetchTokenFile reads the mounted projected token verbatim. It is a
// package variable only so tests can pin the file location.
var readKeyFetchTokenFile = func() (string, error) {
	raw, err := os.ReadFile(ServiceAccountTokenFile)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// authedTransport attaches the key-fetch credential to every request through
// this client — discovery, initial JWKS, and each unknown-kid refresh alike —
// but only within the pinned issuer origin. The credential is a Kubernetes
// identity; the discovery document naming another host does not authorize that
// host to receive it.
type authedTransport struct {
	base   http.RoundTripper
	origin *url.URL
}

func (t authedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !sameOrigin(request.URL, t.origin) {
		return nil, fmt.Errorf("refusing to send the key-fetch credential to %s: outside the pinned issuer origin", request.URL.Host)
	}
	credential, err := keyFetchCredential()
	if err != nil {
		return nil, err
	}
	cloned := request.Clone(request.Context())
	cloned.Header.Set("Authorization", BearerPrefix+credential)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

// sameOrigin compares scheme and host with default ports applied, so an issuer
// written without an explicit port still matches its discovery-served URLs.
func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && a.Scheme == b.Scheme && canonicalHost(a) == canonicalHost(b)
}

func canonicalHost(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Hostname() + ":" + port
}

// oidcDiscovery is the subset of the OIDC discovery document the verifier
// needs.
type oidcDiscovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// applyInClusterAPIServerPort rewrites the port of key-material URLs that
// target the in-cluster apiserver service name. Some managed clusters expose
// the apiserver service on a non-443 port (advertised to Pods through
// KUBERNETES_SERVICE_HOST/PORT) while the issuer anchor keeps the
// conventional port-less form; without this rewrite the discovery and JWKS
// fetches would target a closed port and the Fastlet could never start.
func applyInClusterAPIServerPort(raw string) string {
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if port == "" || port == "443" || host == "" {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() != "kubernetes.default.svc" || parsed.Port() != "" {
		return raw
	}
	parsed.Host = net.JoinHostPort(parsed.Hostname(), port)
	return parsed.String()
}

// fetchDiscovery resolves the issuer's JWKS URI from its OIDC discovery
// document. The issuer is the trust anchor — it comes from this Pod's
// kubelet-written projected token, never from an incoming token — so key
// material cannot be redirected to an attacker-controlled host.
func fetchDiscovery(ctx context.Context, client *http.Client, issuer string) (*oidcDiscovery, error) {
	if issuer == "" {
		return nil, fmt.Errorf("issuer is empty")
	}
	if err := validateKeyMaterialURL(issuer, "issuer"); err != nil {
		return nil, err
	}
	issuerURL, err := url.Parse(issuer)
	if err != nil || issuerURL.Host == "" {
		return nil, fmt.Errorf("issuer %q is not a valid URL", issuer)
	}
	document := &oidcDiscovery{}
	// Some managed clusters expose the apiserver service on a non-443 port
	// (advertised through KUBERNETES_SERVICE_HOST/PORT) while the issuer
	// anchor keeps the conventional port-less form; the fetch URLs follow
	// the advertised port or the discovery/JWKS fetches dead-end on a
	// closed 443.
	discoveryURL := applyInClusterAPIServerPort(strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration")
	if err := fetchJSONDocument(ctx, client, discoveryURL, document); err != nil {
		return nil, err
	}
	if document.Issuer != issuer {
		return nil, fmt.Errorf("discovery issuer %q does not match %q", document.Issuer, issuer)
	}
	if document.JWKSURI == "" {
		return nil, fmt.Errorf("discovery document has no jwks_uri")
	}
	// The discovery document names where signing keys are fetched from. An
	// unvalidated jwks_uri would let an https issuer hand its keys over
	// plaintext, where an on-path attacker can substitute forgeries — apply
	// the same trust-anchor boundary as the issuer itself. Pin the origin too:
	// keys served from another host would receive the key-fetch credential.
	if err := validateKeyMaterialURL(document.JWKSURI, "jwks_uri"); err != nil {
		return nil, err
	}
	// 同源校验两侧都应用端口改写：jwks 与 issuer 的有效拉取地址在
	// 非 443 集群上同为 <host>:<KUBERNETES_SERVICE_PORT>
	jwksURL, parseErr := url.Parse(applyInClusterAPIServerPort(document.JWKSURI))
	issuerFetchURL, parseErr2 := url.Parse(applyInClusterAPIServerPort(issuer))
	if parseErr != nil || parseErr2 != nil || !sameOrigin(jwksURL, issuerFetchURL) {
		return nil, fmt.Errorf("jwks_uri %q is not served by the pinned issuer origin", document.JWKSURI)
	}
	return document, nil
}

// validateKeyMaterialURL enforces the trust-anchor transport boundary: https
// everywhere, with plain http confined to loopback so test servers keep
// working. Both the issuer and the discovery-served jwks_uri anchor
// signature verification and must satisfy it.
func validateKeyMaterialURL(raw, role string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("%s %q is not a valid URL", role, raw)
	}
	if parsed.Scheme == "http" {
		if hostname := parsed.Hostname(); hostname != "127.0.0.1" && hostname != "localhost" && hostname != "::1" {
			return fmt.Errorf("%s %q must use https outside loopback", role, raw)
		}
	}
	return nil
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

// defaultHTTPClient builds the client for issuer key material. The trust set
// is the system roots extended with the injected bundle (CABundleEnv) or the
// projected pod ca.crt, so publicly trusted issuers keep verifying. Every
// request carries the key-fetch credential — discovery is an authenticated
// read, never anonymous — and the transport refuses any request outside the
// issuer origin, so neither redirects nor a foreign jwks_uri can receive the
// credential.
func defaultHTTPClient(issuer string) (*http.Client, error) {
	// The pinned transport origin follows the effective fetch address
	// (in-cluster port rewrite applied); the issuer anchor used for token
	// iss validation stays the original port-less form.
	origin, err := url.Parse(applyInClusterAPIServerPort(issuer))
	if err != nil || origin.Host == "" {
		return nil, fmt.Errorf("issuer %q is not a valid URL", issuer)
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Instrumentation may have replaced the default transport; lose the
		// cloned tuning rather than panic at startup.
		base = &http.Transport{}
	} else {
		base = base.Clone()
	}
	pool, found, err := injectedCABundle()
	if err != nil {
		return nil, err
	}
	if found {
		base.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	// The credential is injected inside RoundTrip, below redirect handling, so
	// every hop would re-receive it — undoing the standard library's
	// cross-host and downgrade Authorization stripping. Refuse to leave the
	// anchor host or scheme; the non-200 redirect fails the fetcher closed.
	return &http.Client{
		Transport: authedTransport{base: base, origin: origin},
		Timeout:   5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxKeyFetchRedirects {
				return fmt.Errorf("stopped after %d redirects", maxKeyFetchRedirects)
			}
			if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}, nil
}

// injectedCABundle resolves the CA trust set: the system roots extended with
// the injected base64 bundle (CABundleEnv), or with the projected pod ca.crt
// when no bundle is injected; found=false leaves the transport on its
// system-roots default. The bundle extends rather than replaces the system
// roots, so issuers with publicly trusted certificates verify alongside
// cluster-internal ones. A set-but-malformed bundle is a configuration
// error, not a degradation: the bundle is the trust anchor for signature
// verification, so silently falling back to a weaker set would betray the
// fail-closed posture.
func injectedCABundle() (pool *x509.CertPool, found bool, err error) {
	if encoded := strings.TrimSpace(os.Getenv(CABundleEnv)); encoded != "" {
		raw, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			return nil, false, fmt.Errorf("%s is set but is not base64-encoded", CABundleEnv)
		}
		pool, err := systemRootsExtendedWith(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s is set but carries no parseable PEM certificate", CABundleEnv)
		}
		return pool, true, nil
	}
	if raw, readErr := os.ReadFile(serviceAccountCAFile); readErr == nil {
		if pool, err := systemRootsExtendedWith(raw); err == nil {
			return pool, true, nil
		}
	}
	return nil, false, nil
}

// systemRootsExtendedWith returns the system root pool with the PEM bundle
// appended. Containers without usable system roots (scratch images) degrade
// to the bundle alone rather than failing startup.
func systemRootsExtendedWith(pemBundle []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBundle) {
		return nil, fmt.Errorf("CA bundle carries no PEM certificate")
	}
	return pool, nil
}
