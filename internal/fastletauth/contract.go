package fastletauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Audience is the TokenRequest audience of every Fastlet control token. It
// scopes the credential to this consumer: a token minted for another audience
// (including the API server's own audience) is rejected, so a caller cannot
// reuse an unrelated ServiceAccount token against a Fastlet.
const Audience = "fast-sandbox-fastlet"

// BearerPrefix is the accepted Authorization scheme prefix.
const BearerPrefix = "Bearer "

// AllowedSubjectsEnv is the environment variable holding the comma-separated
// ServiceAccount subjects allowed to call the Fastlet control API. It
// narrows the built-in platform default; it never disables authentication.
const AllowedSubjectsEnv = "FASTLET_AUTH_ALLOWED_SUBJECTS"

// NamespaceEnv is the environment variable overriding the caller Pod's
// namespace when it runs outside a Pod.
const NamespaceEnv = "FASTLET_AUTH_NAMESPACE"

// ServiceAccountEnv is the environment variable overriding the caller Pod's
// ServiceAccount name when it runs outside a Pod.
const ServiceAccountEnv = "FASTLET_AUTH_SERVICE_ACCOUNT"

// IssuerEnv is the environment variable holding the issuer anchor injected
// into Fastlet Pods by the Pool Controller. A local override covers
// processes running outside a Pod.
const IssuerEnv = "FASTLET_AUTH_ISSUER"

// Auth error codes carried in the middleware's JSON error envelope. Their
// string values match the FastletError codes in internal/protocol/fastlet;
// a test in the protocol package locks the two spellings together.
const (
	CodeAuthRejected    = "AuthRejected"
	CodeAuthNotAllowed  = "AuthNotAllowed"
	CodeAuthUnavailable = "AuthUnavailable"
)

// CABundleEnv is the environment variable holding the base64-encoded cluster
// CA bundle that authenticates TLS to the issuer, injected alongside
// IssuerEnv.
const CABundleEnv = "FASTLET_AUTH_CA_BUNDLE"

// AudienceEnv is the environment variable overriding the token audience on
// both sides for non-default deployments.
const AudienceEnv = "FASTLET_AUTH_AUDIENCE"

var (
	// ErrUnauthenticated reports a missing, malformed, expired, or
	// incorrectly signed token.
	ErrUnauthenticated = errors.New("fastlet auth: unauthenticated")
	// ErrUnauthorized reports a valid token whose ServiceAccount subject is
	// not in the allow list.
	ErrUnauthorized = errors.New("fastlet auth: subject is not permitted")
	// ErrUnavailable reports a transient failure to validate a token, such
	// as the OIDC discovery or JWKS endpoint being unreachable. Callers map
	// it to a retriable server error, not to a rejection of the caller.
	ErrUnavailable = errors.New("fastlet auth: validator unavailable")
)

// ParseSubjects splits a comma-separated ServiceAccount subject list,
// trimming whitespace and dropping empty entries. It is the shared parser
// for the control-plane flag that seeds the Fastlet allow list and for the
// Fastlet-side environment override.
func ParseSubjects(raw string) []string {
	var subjects []string
	for _, candidate := range strings.Split(raw, ",") {
		if subject := strings.TrimSpace(candidate); subject != "" {
			subjects = append(subjects, subject)
		}
	}
	return subjects
}

// SubjectsFlag is the controller flag (and its environment fallback) holding
// the comma-separated ServiceAccount subjects allowed to call Fastlets. The
// Pool Controller injects the parsed list into every Fastlet Pod.
const SubjectsFlag = "fastlet-auth-subjects"

// SubjectsEnv is the environment fallback of SubjectsFlag.
const SubjectsEnv = "FASTLET_AUTH_SUBJECTS"

// BearerToken extracts the bearer token from an Authorization header. It
// returns false when the header is absent, uses another scheme, or carries an
// empty credential.
func BearerToken(header http.Header) (string, bool) {
	value := header.Get("Authorization")
	if !strings.HasPrefix(value, BearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(value, BearerPrefix))
	return token, token != ""
}

// TokenSource supplies the bearer token presented to Fastlets. Implementations
// must be safe for concurrent use and must return short-lived credentials,
// refreshing them without caller involvement.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}
