package fastletauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// exemptPaths are served without authentication. Health probes are scraped
// by the kubelet without a token, metrics are scrape-only, and the heartbeat
// reports node health and inventory to the control plane: it grants no
// intent-changing authority and diagnostics tooling relies on it staying
// open. Every other path, including the whole /api prefix, requires a valid
// allow-listed ServiceAccount token.
var exemptPaths = map[string]struct{}{
	"/livez":                    {},
	"/readyz":                   {},
	"/metrics":                  {},
	"/api/v2/fastlet/heartbeat": {},
}

// type subjectContextKey is the context key of an authenticated caller.
type subjectContextKey struct{}

// SubjectFrom returns the authenticated caller subject previously attached by
// Middleware, or an empty string.
func SubjectFrom(ctx context.Context) string {
	subject, _ := ctx.Value(subjectContextKey{}).(string)
	return subject
}

// Middleware enforces Fastlet control authentication on every request except
// the exempt health and heartbeat endpoints. Responses:
//
//   - 401 when the request carries no valid bearer token;
//   - 403 when the token is valid but its ServiceAccount is not allow-listed;
//   - 503 when validation itself is unavailable (issuer unreachable), so
//     callers retry instead of treating the call as rejected;
//   - otherwise the request proceeds with the subject attached to its context.
//
// Every rejection carries a machine-readable JSON error envelope so the
// control-plane client can classify the outcome — the auth codes deliberately
// match the FastletError codes — and never lose the HTTP status inside a
// decode failure.
func Middleware(authenticator *Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, exempt := exemptPaths[request.URL.Path]; exempt {
			next.ServeHTTP(writer, request)
			return
		}
		token, found := BearerToken(request.Header)
		if !found {
			writeAuthError(writer, http.StatusUnauthorized, CodeAuthRejected, "fastlet control authentication is required")
			return
		}
		subject, err := authenticator.Authenticate(request.Context(), token)
		switch {
		case err == nil:
		case errors.Is(err, ErrUnauthenticated):
			writeAuthError(writer, http.StatusUnauthorized, CodeAuthRejected, "fastlet control token was rejected")
			return
		case errors.Is(err, ErrUnauthorized):
			writeAuthError(writer, http.StatusForbidden, CodeAuthNotAllowed, "fastlet control caller is not permitted")
			return
		default:
			// ErrUnavailable and anything unexpected: the caller may be
			// legitimate, so do not translate the failure into a rejection.
			writeAuthError(writer, http.StatusServiceUnavailable, CodeAuthUnavailable, "fastlet control authentication is temporarily unavailable")
			return
		}
		request = request.WithContext(context.WithValue(request.Context(), subjectContextKey{}, subject))
		next.ServeHTTP(writer, request)
	})
}

// writeAuthError writes the middleware's JSON error envelope. The envelope
// mirrors the FastletError wire shape ("error" object with code, message,
// retryable) so a control-plane client decoding its typed response populates
// the Error field without any cross-package coupling.
func writeAuthError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		writer.Header().Set("WWW-Authenticate", BearerPrefix)
	}
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   message,
			"retryable": status == http.StatusServiceUnavailable,
		},
	})
}
