// Package ublkd provides the shared read-only portion of the OverlayBD control protocol.
package ublkd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Device struct {
	DevID    int    `json:"dev_id"`
	Dev      string `json:"dev"`
	Config   string `json:"config"`
	Writable bool   `json:"writable"`
	State    string `json:"state"`
	Mode     string `json:"mode"`
	RefCount int    `json:"refcount"`
}

func List(ctx context.Context, client *http.Client) ([]Device, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ublkd/v1/list", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ublkd list HTTP status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("ublkd list exceeds response limit")
	}
	var listing struct {
		OK      bool     `json:"ok"`
		Devices []Device `json:"devices"`
		Error   string   `json:"error"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil, err
	}
	if !listing.OK {
		return nil, fmt.Errorf("ublkd list: %s", listing.Error)
	}
	return listing.Devices, nil
}

func ListSocket(ctx context.Context, socket string) ([]Device, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	return List(ctx, &http.Client{Transport: transport, Timeout: 10 * time.Second})
}
