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

// defaultHTTPClient builds the client for issuer key material, trusting the
// injected CA bundle (CABundleEnv) or the projected pod ca.crt; managed-
// cluster issuers verify through the system roots fallback.
func defaultHTTPClient() (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Instrumentation may have replaced the default transport; lose the
		// cloned tuning rather than panic at startup.
		base = &http.Transport{}
	} else {
		base = base.Clone()
	}
	pool, err := injectedCABundle()
	if err != nil {
		return nil, err
	}
	if pool != nil {
		base.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &http.Client{Transport: base, Timeout: 5 * time.Second}, nil
}

// injectedCABundle resolves the CA trust set: injected base64 bundle first,
// projected pod ca.crt second. A nil pool leaves the system roots. A set-but-
// malformed bundle is a configuration error, not a degradation: the bundle is
// the trust anchor for signature verification, so silently falling back to a
// weaker set would betray the fail-closed posture.
func injectedCABundle() (*x509.CertPool, error) {
	if encoded := strings.TrimSpace(os.Getenv(CABundleEnv)); encoded != "" {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(raw) {
			return nil, fmt.Errorf("%s is set but is not parseable as base64-encoded PEM", CABundleEnv)
		}
		return pool, nil
	}
	if raw, err := os.ReadFile(serviceAccountCAFile); err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(raw) {
			return pool, nil
		}
	}
	return nil, nil
}
