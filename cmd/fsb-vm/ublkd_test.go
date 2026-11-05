package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// acquireClient wires Acquire/Release to a fake transport, no daemon needed.
func acquireClient(t *testing.T, handle fenceTransport) *ublkdClient {
	t.Helper()
	return &ublkdClient{httpClient: &http.Client{Transport: handle}}
}

func TestAcquireSendsExclusiveLease(t *testing.T) {
	var method, path string
	var body map[string]string
	client := acquireClient(t, func(r *http.Request) (*http.Response, error) {
		method, path = r.Method, r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		return fenceReply(200, `{"ok":true,"dev_id":7,"dev":"/dev/ublkb7","mode":"exclusive","refcount":1}`), nil
	})

	devID, dev, err := client.Acquire(context.Background(), "/state/sb/rootfs.config.v1.json")
	require.NoError(t, err)
	require.Equal(t, 7, devID)
	require.Equal(t, "/dev/ublkb7", dev)
	require.Equal(t, http.MethodPost, method)
	require.Equal(t, "/v1/acquire", path)
	require.Equal(t, "/state/sb/rootfs.config.v1.json", body[ublkdConfigKey])
	require.Equal(t, "exclusive", body["mode"])
}

func TestAcquireReportsDaemonError(t *testing.T) {
	client := acquireClient(t, func(*http.Request) (*http.Response, error) {
		return fenceReply(200, `{"ok":false,"error":"no pooled device matched"}`), nil
	})

	_, _, err := client.Acquire(context.Background(), "/state/sb/rootfs.config.v1.json")
	require.ErrorContains(t, err, "no pooled device matched")
}

// A lost response leaves the lease outcome unknown: keep the config and roll
// back by listing devices.
func TestAcquireUnknownOutcomeWhenResponseLost(t *testing.T) {
	client := acquireClient(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("lost response")
	})

	_, _, err := client.Acquire(context.Background(), "/state/sb/rootfs.config.v1.json")
	require.ErrorIs(t, err, errAddOutcomeUnknown)
}

// A failed connect means the daemon never saw the request.
func TestAcquireNotSentIsNotUnknown(t *testing.T) {
	client := acquireClient(t, func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	})

	_, _, err := client.Acquire(context.Background(), "/state/sb/rootfs.config.v1.json")
	require.Error(t, err)
	require.NotErrorIs(t, err, errAddOutcomeUnknown)
}

func TestReleaseSendsDevID(t *testing.T) {
	var method, path string
	var body map[string]int
	client := acquireClient(t, func(r *http.Request) (*http.Response, error) {
		method, path = r.Method, r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		return fenceReply(200, `{"ok":true,"refcount":0}`), nil
	})

	require.NoError(t, client.Release(context.Background(), 7))
	require.Equal(t, http.MethodPost, method)
	require.Equal(t, "/v1/release", path)
	require.Equal(t, 7, body["dev_id"])
}

func TestReleaseReportsDaemonError(t *testing.T) {
	client := acquireClient(t, func(*http.Request) (*http.Response, error) {
		return fenceReply(404, `{"ok":false,"error":"no such device: 7"}`), nil
	})

	err := client.Release(context.Background(), 7)
	require.ErrorContains(t, err, "no such device: 7")
}
