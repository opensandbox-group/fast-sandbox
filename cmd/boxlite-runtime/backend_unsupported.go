//go:build !boxlite_native

package main

import (
	"context"

	boxliteprotocol "fast-sandbox/internal/runtime/boxlite/protocol"
	boxliteserver "fast-sandbox/internal/runtime/boxlite/server"
)

type unsupportedBackend struct{}

func newBackend(string) (boxliteserver.Backend, error) { return unsupportedBackend{}, nil }

func (unsupportedBackend) Capabilities(context.Context) boxliteprotocol.Capabilities {
	return boxliteprotocol.Capabilities{
		ProtocolVersion: boxliteprotocol.ProtocolVersionV1,
		Ready:           false, Reason: "BoxLiteNativeBackendNotBuilt",
		Message:      "boxlite-runtime must be built with the boxlite_native tag",
		Capabilities: map[string]bool{},
	}
}

func (unsupportedBackend) Ensure(context.Context, boxliteprotocol.EnsureRequest) (boxliteprotocol.Box, error) {
	return boxliteprotocol.Box{}, unavailable()
}
func (unsupportedBackend) Inspect(context.Context, string) (boxliteprotocol.Box, error) {
	return boxliteprotocol.Box{}, unavailable()
}
func (unsupportedBackend) Recover(context.Context, string) (boxliteprotocol.Box, error) {
	return boxliteprotocol.Box{}, unavailable()
}
func (unsupportedBackend) Delete(context.Context, string) error { return unavailable() }
func (unsupportedBackend) List(context.Context, string) ([]boxliteprotocol.Box, error) {
	return nil, unavailable()
}
func (unsupportedBackend) ListImages(context.Context) ([]string, error) { return nil, unavailable() }
func (unsupportedBackend) PullImage(context.Context, string) error      { return unavailable() }

func unavailable() error {
	return &boxliteserver.Error{Code: boxliteprotocol.ErrorUnavailable, Message: "BoxLite native backend is not built"}
}
