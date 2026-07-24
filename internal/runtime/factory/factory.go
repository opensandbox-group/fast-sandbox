package factory

import (
	"context"
	"fmt"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	boxlitedriver "fast-sandbox/internal/runtime/boxlite/driver"
	"fast-sandbox/internal/runtime/containerd"
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

func (f *Factory) Create(ctx context.Context, runtimeName apiv1alpha1.RuntimeName, socketPath string) (RuntimeDriver, CapabilityReport, error) {
	profile, err := f.catalog.Resolve(runtimeName)
	if err != nil {
		return nil, CapabilityReport{}, err
	}
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
		return nil, report, fmt.Errorf("%w: %v", ErrRuntimeCapabilityUnavailable, err)
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
	case runtimecatalog.DriverKindBoxLite:
		return boxlitedriver.New(profile)
	default:
		return nil, fmt.Errorf("%w: driver kind %q", ErrUnsupportedRuntime, profile.Driver)
	}
}
