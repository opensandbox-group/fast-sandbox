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

	"k8s.io/klog/v2"
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
	parsed, err := url.Parse(issuer)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, fmt.Errorf("issuer %q is not a valid URL", issuer)
	}
	// The issuer anchors signature verification: a plaintext anchor outside
	// loopback would let an on-path attacker serve forged keys. Plain http
	// stays available for loopback test servers only.
	if parsed.Scheme == "http" {
		if hostname := parsed.Hostname(); hostname != "127.0.0.1" && hostname != "localhost" && hostname != "::1" {
			return nil, fmt.Errorf("issuer %q must use https outside loopback", issuer)
		}
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
func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if pool, ok := injectedCABundle(); ok {
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

// injectedCABundle resolves the CA trust set: injected base64 bundle first,
// projected pod ca.crt second; ok=false leaves the system roots.
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
