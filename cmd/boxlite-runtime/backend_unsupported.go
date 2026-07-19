//go:build !boxlite_native

package main

import (
	"context"

	"fast-sandbox/internal/boxlitesidecar"
	"fast-sandbox/internal/boxlitewire"
)

type unsupportedBackend struct{}

func newBackend(string) (boxlitesidecar.Backend, error) { return unsupportedBackend{}, nil }

func (unsupportedBackend) Capabilities(context.Context) boxlitewire.Capabilities {
	return boxlitewire.Capabilities{
		ProtocolVersion: boxlitewire.ProtocolVersionV1,
		Ready:           false, Reason: "BoxLiteNativeBackendNotBuilt",
		Message:      "boxlite-runtime must be built with the boxlite_native tag",
		Capabilities: map[string]bool{},
	}
}

func (unsupportedBackend) Ensure(context.Context, boxlitewire.EnsureRequest) (boxlitewire.Box, error) {
	return boxlitewire.Box{}, unavailable()
}
func (unsupportedBackend) Inspect(context.Context, string) (boxlitewire.Box, error) {
	return boxlitewire.Box{}, unavailable()
}
func (unsupportedBackend) Recover(context.Context, string) (boxlitewire.Box, error) {
	return boxlitewire.Box{}, unavailable()
}
func (unsupportedBackend) Delete(context.Context, string) error { return unavailable() }
func (unsupportedBackend) List(context.Context, string) ([]boxlitewire.Box, error) {
	return nil, unavailable()
}
func (unsupportedBackend) ListImages(context.Context) ([]string, error) { return nil, unavailable() }
func (unsupportedBackend) PullImage(context.Context, string) error      { return unavailable() }

func unavailable() error {
	return &boxlitesidecar.Error{Code: boxlitewire.ErrorUnavailable, Message: "BoxLite native backend is not built"}
}
