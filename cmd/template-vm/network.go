package main

import (
	"context"
	"fmt"
)

// networkProvider acquires the host network device a restored VM attaches to
// (ADR-010). The concrete integration with the cnid-* networking stack is
// pending input from the networking side; the MVP ships only the "none"
// provider so the snapshot-resume main path is unblocked.
type networkProvider interface {
	// Acquire prepares the host device for the guest described by metadata
	// and returns the resolved host device name plus a cleanup callback run
	// on VM teardown. The none provider returns an empty device and a no-op.
	Acquire(ctx context.Context, metadata vmMetadata) (hostDevName string, cleanup func() error, err error)
	// Name identifies the provider (persisted in state.json).
	Name() string
}

// noneProvider skips networking entirely.
type noneProvider struct{}

func (noneProvider) Acquire(context.Context, vmMetadata) (string, func() error, error) {
	return "", func() error { return nil }, nil
}

func (noneProvider) Name() string { return networkNone }

const networkNone = "none"

// selectNetworkProvider resolves the --network flag to a provider. Only
// "none" is supported in the MVP; any other value is rejected until the
// networking integration lands.
func selectNetworkProvider(mode string) (networkProvider, error) {
	switch mode {
	case "", networkNone:
		return noneProvider{}, nil
	default:
		return nil, fmt.Errorf("unsupported --network %q: only %q is implemented", mode, networkNone)
	}
}
