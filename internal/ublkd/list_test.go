package ublkd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListRejectsHTTPErrorEvenWithSuccessBody(t *testing.T) {
	for _, status := range []int{200, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"ok":true,"devices":[{"dev":"/dev/ublkb0","writable":true}]}`))
		}))
		client := server.Client()
		client.Transport = redirectTransport{server.URL, http.DefaultTransport}
		devices, err := List(t.Context(), client)
		if status == 200 {
			require.NoError(t, err)
			require.Len(t, devices, 1)
		} else {
			require.Error(t, err)
		}
		server.Close()
	}
}

type redirectTransport struct {
	url       string
	transport http.RoundTripper
}

func (r redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	replacement, err := http.NewRequestWithContext(req.Context(), req.Method, r.url+req.URL.Path, nil)
	if err != nil {
		return nil, err
	}
	return r.transport.RoundTrip(replacement)
}
