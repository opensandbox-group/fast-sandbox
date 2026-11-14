package fastletauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Audience is the TokenRequest audience of every Fastlet control token: a
// token minted for another audience is rejected, so unrelated ServiceAccount
// tokens cannot be reused against a Fastlet.
const Audience = "fast-sandbox-fastlet"

// BearerPrefix is the accepted Authorization scheme prefix.
const BearerPrefix = "Bearer "

// Env vars. AllowedSubjectsEnv replaces (never disables) the built-in allow
// list. IssuerEnv/CABundleEnv are injected by the Pool Controller into
// Fastlet Pods and double as overrides for processes outside a Pod.
// NamespaceEnv/ServiceAccountEnv/AudienceEnv are likewise non-Pod overrides.
const (
	AllowedSubjectsEnv = "FASTLET_AUTH_ALLOWED_SUBJECTS"
	NamespaceEnv       = "FASTLET_AUTH_NAMESPACE"
	ServiceAccountEnv  = "FASTLET_AUTH_SERVICE_ACCOUNT"
	CABundleEnv        = "FASTLET_AUTH_CA_BUNDLE"
	IssuerEnv          = "FASTLET_AUTH_ISSUER"
	AudienceEnv        = "FASTLET_AUTH_AUDIENCE"
)

// Auth error codes carried in the middleware's JSON error envelope; the
// string values match the FastletError codes in internal/protocol/fastlet
// (locked by a protocol-package test).
const (
	CodeAuthRejected    = "AuthRejected"
	CodeAuthNotAllowed  = "AuthNotAllowed"
	CodeAuthUnavailable = "AuthUnavailable"
)

var (
	// ErrUnauthenticated reports a missing, malformed, expired, or
	// incorrectly signed token.
	ErrUnauthenticated = errors.New("fastlet auth: unauthenticated")
	// ErrUnauthorized reports a valid token whose subject is not allowed.
	ErrUnauthorized = errors.New("fastlet auth: subject is not permitted")
	// ErrUnavailable reports a transient validation failure (issuer
	// unreachable); callers map it to a retriable 503, not a rejection.
	ErrUnavailable = errors.New("fastlet auth: validator unavailable")
)

// ParseSubjects splits a comma-separated subject list, dropping whitespace
// and empty entries. Shared by the control-plane flag and the Fastlet-side
// env override.
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

// TokenSource supplies the bearer token presented to Fastlets.
// Implementations must be safe for concurrent use and refresh their
// credentials without caller involvement.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}
