package runtime

import (
	"context"
	"fmt"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/runtimecatalog"
)

type DriverFactory struct {
	catalog *runtimecatalog.Catalog
	prober  CapabilityProber
}

func NewDriverFactory(catalog *runtimecatalog.Catalog, prober CapabilityProber) *DriverFactory {
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	if prober == nil {
		prober = NewHostCapabilityProber()
	}
	return &DriverFactory{catalog: catalog, prober: prober}
}

func (f *DriverFactory) Create(ctx context.Context, runtimeName apiv1alpha1.RuntimeName, socketPath string) (RuntimeDriver, CapabilityReport, error) {
	profile, err := f.catalog.Resolve(runtimeName)
	if err != nil {
		return nil, CapabilityReport{}, err
	}
	report := f.prober.Probe(ctx, profile, socketPath)
	if report.State == runtimecatalog.CapabilityUnsupported || report.State == runtimecatalog.CapabilityDegraded {
		return nil, report, fmt.Errorf("%w: %s: %s", ErrRuntimeCapabilityUnavailable, report.Reason, report.Message)
	}

	driver, err := buildRuntimeDriver(profile)
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

func buildRuntimeDriver(profile runtimecatalog.RuntimeProfile) (RuntimeDriver, error) {
	switch profile.Driver {
	case runtimecatalog.DriverKindContainerd:
		if profile.Containerd == nil {
			return nil, fmt.Errorf("containerd runtime profile %q has no private configuration", profile.Name)
		}
		cfg := RuntimeConfig{
			Handler: profile.Containerd.Handler, RuntimePath: profile.Containerd.RuntimePath,
			ConfigPath: profile.Containerd.ConfigPath, NeedsTTY: profile.Containerd.NeedsTTY,
			OptionsType: profile.Containerd.OptionsType,
		}
		return newContainerdRuntimeWithConfig(profile.Name, cfg), nil
	case runtimecatalog.DriverKindBoxLite:
		return nil, fmt.Errorf("%w: BoxLiteDriverNotImplemented", ErrUnsupportedRuntime)
	default:
		return nil, fmt.Errorf("%w: driver kind %q", ErrUnsupportedRuntime, profile.Driver)
	}
}
