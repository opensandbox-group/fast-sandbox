// Package fastletauthtest provides a local OIDC issuer for tests of Fastlet
// control authentication. It serves an OIDC discovery document and a JWKS
// endpoint and mints signed JWTs, so verifiers exercise their real fetch,
// parse, and signature paths without a Kubernetes API server.
package fastletauthtest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Default keys published by a new issuer.
const (
	DefaultRSAKeyID = "test-rsa"
	DefaultECKeyID  = "test-ec"
)

// Issuer is a local OIDC issuer bound to the test lifetime.
type Issuer struct {
	server  *httptest.Server
	rsaKey  *rsa.PrivateKey
	ecKey   *ecdsa.PrivateKey
	fetches atomic.Int64

	mu   sync.Mutex
	keys map[string]publishedKey
}

// publishedKey is one key the JWKS endpoint serves.
type publishedKey struct {
	kid    string
	signer crypto.Signer
}

// NewIssuer starts a local issuer with a default RSA and EC signing key
// published under DefaultRSAKeyID and DefaultECKeyID.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA test key: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC test key: %v", err)
	}
	issuer := &Issuer{
		rsaKey: rsaKey,
		ecKey:  ecKey,
		keys: map[string]publishedKey{
			DefaultRSAKeyID: {kid: DefaultRSAKeyID, signer: rsaKey},
			DefaultECKeyID:  {kid: DefaultECKeyID, signer: ecKey},
		},
	}
	issuer.mu = sync.Mutex{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   issuer.server.URL,
			"jwks_uri": issuer.server.URL + "/openid/v1/jwks",
		})
	})
	mux.HandleFunc("/openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		issuer.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(issuer.jwksDocument())
	})
	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	return issuer
}

// URL is the issuer URL served by the discovery document.
func (i *Issuer) URL() string { return i.server.URL }

// Close shuts the issuer down early (tests normally rely on t.Cleanup).
func (i *Issuer) Close() { i.server.Close() }

// Fetches counts discovery plus JWKS requests served so far.
func (i *Issuer) Fetches() int64 { return i.fetches.Load() }

// PublishKey adds or replaces a published signing key.
func (i *Issuer) PublishKey(kid string, signer crypto.Signer) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keys[kid] = publishedKey{kid: kid, signer: signer}
}

// RemoveKey stops publishing a key without affecting tokens already minted
// with it.
func (i *Issuer) RemoveKey(kid string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.keys, kid)
}

// MintOptions selects the token a Mint call produces. Zero values select a
// valid, currently active RS256 token for the platform Fast-Path subject and
// audience signed by the default RSA key.
type MintOptions struct {
	Kid       string
	Algorithm string
	Subject   string
	Audience  string
	// AudienceScalar emits the audience as a JSON string instead of an
	// array (RFC 7519 allows both).
	AudienceScalar bool
	// OmitExp builds the payload without an exp claim (never-expiring
	// legacy token shape).
	OmitExp   bool
	Issuer    string
	ExpiresAt int64
	NotBefore int64
	// Signer overrides the signing key; it need not be published.
	Signer crypto.Signer
	// Segments overrides the emitted token text entirely (for malformed
	// token tests).
	Segments []string
}

// Mint returns a signed token per the options.
func (i *Issuer) Mint(options MintOptions) string {
	if len(options.Segments) > 0 {
		return joinSegments(options.Segments)
	}
	if options.Kid == "" {
		options.Kid = DefaultRSAKeyID
	}
	if options.Algorithm == "" {
		options.Algorithm = "RS256"
	}
	if options.Subject == "" {
		options.Subject = "system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath"
	}
	if options.Audience == "" {
		options.Audience = "fast-sandbox-fastlet"
	}
	if options.Issuer == "" {
		options.Issuer = i.URL()
	}
	now := time.Now().Unix()
	if options.ExpiresAt == 0 {
		options.ExpiresAt = now + 3600
	}
	if options.NotBefore == 0 {
		options.NotBefore = now
	}
	signer := options.Signer
	if signer == nil {
		i.mu.Lock()
		entry, found := i.keys[options.Kid]
		i.mu.Unlock()
		if !found {
			panic(fmt.Sprintf("fastletauthtest: key %q is not published; pass Signer to mint with an unpublished key", options.Kid))
		}
		signer = entry.signer
	}
	header := map[string]any{"alg": options.Algorithm, "kid": options.Kid, "typ": "JWT"}
	audienceClaim := any([]string{options.Audience})
	if options.AudienceScalar {
		audienceClaim = options.Audience
	}
	claims := map[string]any{
		"iss": options.Issuer,
		"sub": options.Subject,
		"aud": audienceClaim,
		"nbf": options.NotBefore,
		"iat": now,
	}
	if !options.OmitExp {
		claims["exp"] = options.ExpiresAt
	}
	headerSegment := encodeSegment(header)
	claimsSegment := encodeSegment(claims)
	signingInput := headerSegment + "." + claimsSegment
	signature, err := sign(signer, []byte(signingInput))
	if err != nil {
		panic(fmt.Sprintf("fastletauthtest: sign token: %v", err))
	}
	return signingInput + "." + signature
}

func (i *Issuer) jwksDocument() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()
	keys := make([]map[string]any, 0, len(i.keys))
	for _, entry := range i.keys {
		keys = append(keys, jwksEntry(entry.kid, entry.signer))
	}
	return map[string]any{"keys": keys}
}

func jwksEntry(kid string, signer crypto.Signer) map[string]any {
	switch key := signer.(type) {
	case *rsa.PrivateKey:
		return map[string]any{
			"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}
	case *ecdsa.PrivateKey:
		size := (key.Curve.Params().BitSize + 7) / 8
		return map[string]any{
			"kty": "EC", "kid": kid, "alg": "ES256", "use": "sig", "crv": "P-256",
			"x": base64.RawURLEncoding.EncodeToString(key.PublicKey.X.FillBytes(make([]byte, size))),
			"y": base64.RawURLEncoding.EncodeToString(key.PublicKey.Y.FillBytes(make([]byte, size))),
		}
	default:
		panic(fmt.Sprintf("fastletauthtest: unsupported signer %T", signer))
	}
}

func sign(signer crypto.Signer, message []byte) (string, error) {
	digest := sha256.Sum256(message)
	switch key := signer.(type) {
	case *rsa.PrivateKey:
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			return "", err
		}
		return encodeRaw(signature), nil
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			return "", err
		}
		raw := make([]byte, 64)
		r.FillBytes(raw[:32])
		s.FillBytes(raw[32:])
		return encodeRaw(raw), nil
	default:
		return "", fmt.Errorf("unsupported signer %T", signer)
	}
}

func encodeSegment(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encodeRaw(raw)
}

func encodeRaw(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func joinSegments(segments []string) string {
	joined := segments[0]
	for _, segment := range segments[1:] {
		joined += "." + segment
	}
	return joined
}
