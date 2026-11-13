package fastletauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/fastletauth/fastletauthtest"
)

func newTestMiddleware(t *testing.T) (http.Handler, *fastletauthtest.Issuer) {
	t.Helper()
	issuer := fastletauthtest.NewIssuer(t)
	authenticator := newTestAuthenticator(t, issuer)
	observed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(SubjectFrom(r.Context())))
	})
	return Middleware(authenticator, observed), issuer
}

func TestMiddlewareAcceptsAuthenticatedRequest(t *testing.T) {
	handler, issuer := newTestMiddleware(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v2/fastlet/create", nil)
	request.Header.Set("Authorization", BearerPrefix+issuer.Mint(fastletauthtest.MintOptions{}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath", recorder.Body.String())
}

func TestMiddlewareRejectsUnauthenticatedRequests(t *testing.T) {
	handler, issuer := newTestMiddleware(t)
	cases := []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{"missing header", "", http.StatusUnauthorized, CodeAuthRejected},
		{"wrong scheme", "Basic dXNlcjpwYXNz", http.StatusUnauthorized, CodeAuthRejected},
		{"empty credential", BearerPrefix, http.StatusUnauthorized, CodeAuthRejected},
		{"garbage token", BearerPrefix + "not-a-jwt", http.StatusUnauthorized, CodeAuthRejected},
		{"expired token", BearerPrefix + issuer.Mint(fastletauthtest.MintOptions{ExpiresAt: time.Now().Unix() - 3600}), http.StatusUnauthorized, CodeAuthRejected},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v2/fastlet/create", nil)
			if testCase.token != "" {
				request.Header.Set("Authorization", testCase.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			require.Equal(t, testCase.status, recorder.Code)
			require.NotEmpty(t, recorder.Header().Get("WWW-Authenticate"))
			// The body is a machine-readable envelope so the control-plane
			// client can classify the outcome without losing the status.
			var envelope struct {
				Error struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
			require.Equal(t, testCase.code, envelope.Error.Code)
			require.Equal(t, recorder.Code == http.StatusServiceUnavailable, envelope.Error.Retryable)
		})
	}
}

func TestMiddlewareRejectsNonAllowListedSubject(t *testing.T) {
	handler, issuer := newTestMiddleware(t)
	request := httptest.NewRequest(http.MethodDelete, "/api/v2/fastlet/delete", nil)
	request.Header.Set("Authorization", BearerPrefix+issuer.Mint(fastletauthtest.MintOptions{Subject: "system:serviceaccount:attacker:default"}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestMiddlewareMapsUnavailableValidationToServiceUnavailable(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	authenticator, err := NewAuthenticator(issuer.URL(), Audience, DefaultAllowedSubjects, nil, WithRefreshInterval(time.Millisecond))
	require.NoError(t, err)
	handler := Middleware(authenticator, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// Kill the issuer after construction: a refresh on an unknown signing
	// key now fails, which is a validator fault (503), not a rejection.
	issuer.Close()
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v2/fastlet/create", nil)
	request.Header.Set("Authorization", BearerPrefix+issuer.Mint(fastletauthtest.MintOptions{Kid: "late-key", Signer: otherKey}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestMiddlewareExemptPathsServeWithoutAuthentication(t *testing.T) {
	handler, _ := newTestMiddleware(t)
	for _, path := range []string{"/livez", "/readyz", "/metrics", "/api/v2/fastlet/heartbeat"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, recorder.Code, "exempt path %s must not require a token", path)
		})
	}
}

func TestMiddlewareProtectsEveryOtherAPIPath(t *testing.T) {
	handler, _ := newTestMiddleware(t)
	for _, path := range []string{
		"/api/v2/fastlet/create", "/api/v2/fastlet/delete", "/api/v2/fastlet/inspect",
		"/api/v2/fastlet/snapshots/create", "/api/v2/fastlet/draining",
		"/api/v2/fastlet/runtime-diagnostics", "/api/v2/fastlet/diagnostics/sandbox",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
			require.Equal(t, http.StatusUnauthorized, recorder.Code, "path %s must require a token", path)
		})
	}
}
