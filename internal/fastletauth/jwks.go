package fastletauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
// this client — discovery, initial JWKS, and each unknown-kid refresh alike.
type authedTransport struct {
	base http.RoundTripper
}

func (t authedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
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

// oidcDiscovery is the subset of the OIDC discovery document the verifier
// needs.
type oidcDiscovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
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
	// The discovery document names where signing keys are fetched from. An
	// unvalidated jwks_uri would let an https issuer hand its keys over
	// plaintext, where an on-path attacker can substitute forgeries — apply
	// the same trust-anchor boundary as the issuer itself.
	if err := validateKeyMaterialURL(document.JWKSURI, "jwks_uri"); err != nil {
		return nil, err
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
// request carries the key-fetch credential: discovery is an authenticated
// read, never anonymous.
func defaultHTTPClient() (*http.Client, error) {
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
		Transport: authedTransport{base: base},
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
