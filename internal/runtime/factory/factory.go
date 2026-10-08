package factory

import (
	"context"
	"fmt"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/runtime/containerd"
	firecrackerdriver "fast-sandbox/internal/runtime/firecracker"
)

type Factory struct {
	catalog *runtimecatalog.Catalog
	prober  CapabilityProber
}

func New(catalog *runtimecatalog.Catalog, prober CapabilityProber) *Factory {
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	if prober == nil {
		prober = NewHostCapabilityProber()
	}
	return &Factory{catalog: catalog, prober: prober}
}

// CreateProfile constructs a driver from the immutable runtime plan delivered
// to Fastlet. It deliberately does not resolve the built-in catalog again.
func (f *Factory) CreateProfile(ctx context.Context, profile runtimecatalog.RuntimeProfile, socketPath string) (RuntimeDriver, CapabilityReport, error) {
	report := f.prober.Probe(ctx, profile, socketPath)
	if report.State == runtimecatalog.CapabilityUnsupported || report.State == runtimecatalog.CapabilityDegraded {
		return nil, report, fmt.Errorf("%w: %s: %s", ErrRuntimeCapabilityUnavailable, report.Reason, report.Message)
	}

	driver, err := buildDriver(profile)
	if err != nil {
		report.State = runtimecatalog.CapabilityUnsupported
		report.Reason = "RuntimeDriverUnsupported"
		report.Message = err.Error()
		return nil, report, err
	}
	if err := driver.Initialize(ctx, socketPath); err != nil {
		report.State = runtimecatalog.CapabilityDegraded
		report.Reason = "RuntimeDriverInitializeFailed"
		report.Message = err.Error()
		_ = driver.Close()
		return nil, report, fmt.Errorf("%w: %w", ErrRuntimeCapabilityUnavailable, err)
	}
	report = driver.ProbeCapabilities(ctx)
	if !report.Ready() {
		_ = driver.Close()
		return nil, report, fmt.Errorf("%w: %s: %s", ErrRuntimeCapabilityUnavailable, report.Reason, report.Message)
	}
	return driver, report, nil
}

func buildDriver(profile runtimecatalog.RuntimeProfile) (RuntimeDriver, error) {
	switch profile.Driver {
	case runtimecatalog.DriverKindContainerd:
		return containerd.New(profile)
	case runtimecatalog.DriverKindFirecracker:
		return firecrackerdriver.New(profile)
	default:
		return nil, fmt.Errorf("%w: driver kind %q", ErrUnsupportedRuntime, profile.Driver)
	}
}
