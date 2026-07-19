package routeauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const tokenVersion = "v1"

var (
	ErrInvalidCredential = errors.New("invalid route credential")
	ErrExpiredCredential = errors.New("expired route credential")
	ErrClaimMismatch     = errors.New("route credential claim mismatch")
)

// Claims is the complete authority granted by a route credential. Credentials
// are intentionally instance-specific: a reset, reassignment, or Fastlet Pod
// replacement changes at least one fenced field and invalidates the token.
type Claims struct {
	Tenant            string `json:"tenant,omitempty"`
	Namespace         string `json:"namespace"`
	SandboxUID        string `json:"sandboxUid"`
	TargetPort        uint32 `json:"targetPort"`
	FastletPodUID     string `json:"fastletPodUid"`
	AssignmentAttempt int64  `json:"assignmentAttempt"`
	RouteGeneration   int64  `json:"routeGeneration"`
	ExpiresAt         int64  `json:"expiresAt"`
	Nonce             string `json:"nonce"`
}

func (c Claims) Validate() error {
	if c.Namespace == "" || c.SandboxUID == "" || c.TargetPort == 0 || c.TargetPort > 65535 ||
		c.FastletPodUID == "" || c.AssignmentAttempt <= 0 || c.RouteGeneration <= 0 || c.ExpiresAt <= 0 {
		return fmt.Errorf("%w: required claim is missing", ErrInvalidCredential)
	}
	return nil
}

type Issuer struct {
	privateKey ed25519.PrivateKey
	now        func() time.Time
	ttl        time.Duration
}

func NewIssuer(privateKey ed25519.PrivateKey, ttl time.Duration, now func() time.Time) (*Issuer, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: Ed25519 private key must be %d bytes", ErrInvalidCredential, ed25519.PrivateKeySize)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: credential TTL must be positive", ErrInvalidCredential)
	}
	if now == nil {
		now = time.Now
	}
	return &Issuer{privateKey: append(ed25519.PrivateKey(nil), privateKey...), ttl: ttl, now: now}, nil
}

func (i *Issuer) Issue(claims Claims) (string, Claims, error) {
	if i == nil {
		return "", Claims{}, fmt.Errorf("%w: issuer is not configured", ErrInvalidCredential)
	}
	claims.ExpiresAt = i.now().Add(i.ttl).Unix()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", Claims{}, fmt.Errorf("generate credential nonce: %w", err)
	}
	claims.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	if err := claims.Validate(); err != nil {
		return "", Claims{}, err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", Claims{}, fmt.Errorf("marshal route credential: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signed := tokenVersion + "." + encodedPayload
	signature := ed25519.Sign(i.privateKey, []byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), claims, nil
}

type Verifier struct {
	publicKeys []ed25519.PublicKey
	now        func() time.Time
	leeway     time.Duration
}

func NewVerifier(publicKey ed25519.PublicKey, now func() time.Time) (*Verifier, error) {
	return NewVerifierSet([]ed25519.PublicKey{publicKey}, now)
}

// NewVerifierSet builds a verifier that accepts credentials signed by any key
// in the set. Deployments use this during key rotation: publish old+new public
// keys first, switch the FastPath signer, then remove the old public key after
// the maximum credential TTL has elapsed.
func NewVerifierSet(publicKeys []ed25519.PublicKey, now func() time.Time) (*Verifier, error) {
	if len(publicKeys) == 0 {
		return nil, fmt.Errorf("%w: at least one Ed25519 public key is required", ErrInvalidCredential)
	}
	keys := make([]ed25519.PublicKey, 0, len(publicKeys))
	for _, publicKey := range publicKeys {
		if len(publicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: Ed25519 public key must be %d bytes", ErrInvalidCredential, ed25519.PublicKeySize)
		}
		keys = append(keys, append(ed25519.PublicKey(nil), publicKey...))
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{publicKeys: keys, now: now}, nil
}

func (v *Verifier) WithLeeway(leeway time.Duration) *Verifier {
	if v == nil {
		return nil
	}
	clone := *v
	clone.leeway = leeway
	return &clone
}

func (v *Verifier) Verify(token string) (Claims, error) {
	if v == nil {
		return Claims{}, fmt.Errorf("%w: verifier is not configured", ErrInvalidCredential)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != tokenVersion {
		return Claims{}, ErrInvalidCredential
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !v.verifySignature([]byte(parts[0]+"."+parts[1]), signature) {
		return Claims{}, ErrInvalidCredential
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidCredential
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Validate() != nil {
		return Claims{}, ErrInvalidCredential
	}
	if !v.now().Add(-v.leeway).Before(time.Unix(claims.ExpiresAt, 0)) {
		return Claims{}, ErrExpiredCredential
	}
	return claims, nil
}

func (v *Verifier) verifySignature(message, signature []byte) bool {
	for _, publicKey := range v.publicKeys {
		if ed25519.Verify(publicKey, message, signature) {
			return true
		}
	}
	return false
}

// VerifyExpected validates both the signature/expiry and every route-fencing
// field. Tenant may be omitted by callers that do not yet partition a
// namespace further, but when present it is exact-match authority.
func (v *Verifier) VerifyExpected(token string, expected Claims) (Claims, error) {
	actual, err := v.Verify(token)
	if err != nil {
		return Claims{}, err
	}
	if actual.Namespace != expected.Namespace || actual.SandboxUID != expected.SandboxUID ||
		actual.TargetPort != expected.TargetPort || actual.FastletPodUID != expected.FastletPodUID ||
		actual.AssignmentAttempt != expected.AssignmentAttempt || actual.RouteGeneration != expected.RouteGeneration ||
		(expected.Tenant != "" && actual.Tenant != expected.Tenant) {
		return Claims{}, ErrClaimMismatch
	}
	return actual, nil
}

func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	}
	if err != nil {
		return nil, fmt.Errorf("decode Ed25519 private key: %w", err)
	}
	if len(data) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(data), nil
	}
	if len(data) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("Ed25519 private key must contain a %d-byte seed or %d-byte private key", ed25519.SeedSize, ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(data), nil
}

func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	}
	if err != nil {
		return nil, fmt.Errorf("decode Ed25519 public key: %w", err)
	}
	if len(data) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("Ed25519 public key must contain %d bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(data), nil
}

// ParsePublicKeySet parses a comma-separated list of base64 Ed25519 public
// keys. A single key remains fully backward compatible with existing config.
func ParsePublicKeySet(encoded string) ([]ed25519.PublicKey, error) {
	parts := strings.Split(encoded, ",")
	keys := make([]ed25519.PublicKey, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return nil, errors.New("Ed25519 public key set contains an empty entry")
		}
		key, err := ParsePublicKey(part)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}
