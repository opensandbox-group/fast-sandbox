package factory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

const defaultContainerdSocket = "/run/containerd/containerd.sock"

type HostCapabilityProber struct {
	stat     func(string) (os.FileInfo, error)
	lookPath func(string) (string, error)
	readFile func(string) ([]byte, error)
}

func NewHostCapabilityProber() *HostCapabilityProber {
	return &HostCapabilityProber{stat: os.Stat, lookPath: exec.LookPath, readFile: os.ReadFile}
}

func (p *HostCapabilityProber) Probe(_ context.Context, profile runtimecatalog.RuntimeProfile, socketPath string) CapabilityReport { //nolint:gocognit // pre-existing per-runtime dependency checks; refactor tracked separately
	report := CapabilityReport{
		Runtime: profile.Name, ProfileHash: profile.ProfileHash,
		State: runtimecatalog.CapabilityAvailable,
	}
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityUnsupported {
		report.State = runtimecatalog.CapabilityUnsupported
		report.Reason = profile.Capabilities.Reason
		report.Message = "runtime profile is registered but its production capability gate is not enabled"
		return report
	}
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityDegraded {
		report.State = runtimecatalog.CapabilityDegraded
		report.Reason = profile.Capabilities.Reason
		report.Message = "runtime profile is registered but has not passed the platform validation gate"
		return report
	}

	if profile.Deployment.RequiresKVM {
		p.requirePath(&report, "/dev/kvm", "KVMUnavailable")
	}

	switch profile.Driver {
	case runtimecatalog.DriverKindContainerd:
		if socketPath == "" {
			socketPath = defaultContainerdSocket
		}
		p.requirePath(&report, socketPath, "ContainerdSocketUnavailable")
		if profile.Containerd == nil {
			p.missing(&report, "containerd runtime configuration", "RuntimeProfileInvalid")
			break
		}
		if profile.Containerd.ConfigPath != "" {
			p.requirePath(&report, profile.Containerd.ConfigPath, "RuntimeConfigUnavailable")
		}
		if profile.Name == apiv1alpha2.RuntimeKataClh && profile.Containerd.ConfigPath != "" {
			contents, err := p.readFile(profile.Containerd.ConfigPath)
			if err == nil && !hasActiveTOMLSetting(string(contents), "sandbox_cgroup_only", "true") {
				p.missing(&report, profile.Containerd.ConfigPath+":sandbox_cgroup_only=true", "RuntimeConfigIncompatible")
			}
		}
		if profile.Containerd.RuntimePath != "" {
			p.requirePath(&report, profile.Containerd.RuntimePath, "RuntimeBinaryUnavailable")
		}
		if profile.Name == apiv1alpha2.RuntimeGVisor {
			if _, err := p.lookPath("runsc"); err != nil {
				p.missing(&report, "runsc", "RuntimeBinaryUnavailable")
			}
		}
	case runtimecatalog.DriverKindFirecracker:
		if profile.Firecracker == nil {
			p.missing(&report, "firecracker runtime configuration", "RuntimeProfileInvalid")
			break
		}
		// KernelPath is optional (#84); a pinned path is still probed.
		if profile.Firecracker.BinaryPath == "" || profile.Firecracker.RootfsPath == "" || profile.Firecracker.StateRoot == "" {
			p.missing(&report, "firecracker runtime paths", "RuntimeProfileInvalid")
			break
		}
		if profile.Firecracker.DefaultVCPUs < 1 || profile.Firecracker.DefaultMemory == "" || profile.Firecracker.BootTimeoutSeconds < 1 {
			p.missing(&report, "firecracker boot profile", "RuntimeProfileInvalid")
			break
		}
		p.requirePath(&report, profile.Firecracker.BinaryPath, "RuntimeBinaryUnavailable")
		if profile.Firecracker.KernelPath != "" {
			p.requirePath(&report, profile.Firecracker.KernelPath, "RuntimeKernelUnavailable")
		}
		p.requirePath(&report, profile.Firecracker.StateRoot, "RuntimeStateRootUnavailable")
	default:
		p.missing(&report, string(profile.Driver), "RuntimeDriverUnsupported")
	}

	if len(report.Missing) > 0 {
		report.State = runtimecatalog.CapabilityDegraded
		report.Message = fmt.Sprintf("runtime dependencies are unavailable: %v", report.Missing)
	}
	return report
}

func hasActiveTOMLSetting(contents, key, value string) bool {
	want := key + " = " + value
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if line == want {
			return true
		}
	}
	return false
}

func (p *HostCapabilityProber) requirePath(report *CapabilityReport, path, reason string) {
	if _, err := p.stat(path); err != nil {
		p.missing(report, path, reason)
	}
}

func (p *HostCapabilityProber) missing(report *CapabilityReport, dependency, reason string) {
	if report.Reason == "" {
		report.Reason = reason
	}
	report.Missing = append(report.Missing, dependency)
}
