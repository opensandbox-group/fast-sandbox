package fastletauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"k8s.io/klog/v2"
)

// exemptPaths are served without authentication: kubelet probes have no
// token, metrics are scrape-only, and heartbeat grants no intent-changing
// authority. Everything else, including the whole /api prefix, requires a
// valid allow-listed ServiceAccount token.
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

// Middleware enforces authentication on every request except the exempt
// health and heartbeat endpoints: 401 without a valid token, 403 for a
// non-allow-listed subject, 503 when validation itself is unavailable (the
// caller may be legitimate, so it retries rather than treating the call as
// rejected). The subject is attached to the request context.
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
			// Never translate validator faults into rejections; log the
			// cause server-side since the envelope stays generic.
			klog.ErrorS(err, "Fastlet control authentication is unavailable")
			writeAuthError(writer, http.StatusServiceUnavailable, CodeAuthUnavailable, "fastlet control authentication is temporarily unavailable")
			return
		}
		request = request.WithContext(context.WithValue(request.Context(), subjectContextKey{}, subject))
		next.ServeHTTP(writer, request)
	})
}

// writeAuthError emits the JSON rejection envelope. It mirrors the
// FastletError wire shape, so the control-plane client populates its Error
// field without cross-package coupling.
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
