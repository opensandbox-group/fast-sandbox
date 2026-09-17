package infra

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	infracontract "fast-sandbox/internal/infra/contract"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/sandbox/supervisor"

	"k8s.io/klog/v2"
)

const (
	SandboxInitContainerPath   = "/.fast/bin/sandbox-init"
	SandboxTunnelContainerPath = "/.fast/bin/sandbox-tunnel"
	InstanceConfigPath         = "/.fast/run/infra.json"
)

type Mount struct {
	Source      string   `json:"source"`
	GuestSource string   `json:"guestSource,omitempty"`
	Destination string   `json:"destination"`
	Options     []string `json:"options"`
}

type ServiceEndpoint = infracontract.ServiceEndpoint
type ComponentDiagnostic = infracontract.ComponentDiagnostic

type PreparedInstance struct {
	SandboxUID      string                `json:"sandboxUid"`
	ConfigPodPath   string                `json:"configPodPath"`
	ConfigHostPath  string                `json:"configHostPath"`
	Mounts          []Mount               `json:"mounts"`
	WrapperRequired bool                  `json:"wrapperRequired"`
	Services        []ServiceEndpoint     `json:"services,omitempty"`
	Diagnostics     []ComponentDiagnostic `json:"diagnostics,omitempty"`
}

type persistedInstance struct {
	Version  int               `json:"version"`
	Identity instanceIdentity  `json:"identity"`
	Init     supervisor.Config `json:"sandboxInit"`
	Prepared PreparedInstance  `json:"prepared"`
}

type instanceIdentity struct {
	SandboxUID         string `json:"sandboxUid"`
	InstanceGeneration int64  `json:"instanceGeneration"`
	AssignmentAttempt  int64  `json:"assignmentAttempt"`
}

func (m *Manager) PrepareInstance(ctx context.Context, config *fastletapi.RuntimeSandboxConfig) (_ PreparedInstance, resultErr error) {
	started := time.Now()
	ctx, span := observability.Start(ctx, "fastlet.infra.prepare_instance")
	defer func() {
		observeInstanceStage("total", started, resultErr)
		observability.End(span, resultErr)
	}()
	if config == nil || config.Identity.SandboxUID == "" || config.Identity.InstanceGeneration <= 0 || config.Identity.AssignmentAttempt <= 0 {
		return PreparedInstance{}, errors.New("Sandbox UID, instance generation, and assignment attempt are required for Infra init")
	}
	plan, err := m.Plan()
	if err != nil {
		return PreparedInstance{}, err
	}
	if plan.Revision != config.Spec.InfraRevision {
		return PreparedInstance{}, errors.New("Sandbox Infra revision does not match prepared plan")
	}
	if len(plan.Components) == 0 && plan.Tunnel == nil {
		return PreparedInstance{SandboxUID: config.Identity.SandboxUID}, nil
	}

	result := PreparedInstance{SandboxUID: config.Identity.SandboxUID}
	if plan.Tunnel != nil {
		result.Mounts = append(result.Mounts, Mount{
			Source: plan.Tunnel.HostPath, GuestSource: plan.Tunnel.PodPath,
			Destination: SandboxTunnelContainerPath, Options: []string{"ro", "nosuid", "nodev"},
		})
	}
	if len(plan.Components) == 0 {
		return result, nil
	}
	hasGuestComponents := false
	for _, prepared := range plan.Components {
		if prepared.Plan.Delivery != runtimecatalog.InfraDeliveryHostProcess {
			hasGuestComponents = true
			break
		}
	}
	if hasGuestComponents {
		if plan.Supervisor == nil {
			return PreparedInstance{}, errors.New("sandbox-init is not prepared")
		}
		result.WrapperRequired = true
		result.Mounts = append(result.Mounts, Mount{
			Source: plan.Supervisor.HostPath, GuestSource: plan.Supervisor.PodPath,
			Destination: SandboxInitContainerPath, Options: []string{"ro", "rbind", "nosuid", "nodev"},
		})
	}

	initConfig := supervisor.Config{Version: supervisor.ConfigVersion, SandboxUID: config.Identity.SandboxUID}
	for _, prepared := range plan.Components {
		component := prepared.Plan
		if component.Delivery == runtimecatalog.InfraDeliveryHostProcess {
			klog.InfoS("infra host-process component registered for Pod-loopback readiness",
				"component", component.Name, "port", component.Endpoint.Port,
				"sandboxID", config.Identity.SandboxUID, "probe", component.Process.Readiness.Type)
			result.Services = append(result.Services, ServiceEndpoint{
				Component: component.Name, Protocol: component.Endpoint.Protocol,
				Port: component.Endpoint.Port, Readiness: component.Process.Readiness, HostProcess: true,
			})
			result.Diagnostics = append(result.Diagnostics, ComponentDiagnostic{Component: component.Name, State: "Starting"})
			continue
		}
		for _, mapping := range prepared.Mappings {
			result.Mounts = append(result.Mounts, Mount{
				Source: mapping.HostPath, GuestSource: mapping.PodPath,
				Destination: mapping.TargetPath, Options: []string{"ro", "rbind", "nosuid", "nodev"},
			})
		}
		environment := map[string]string{
			"FAST_SANDBOX_UID":                 config.Identity.SandboxUID,
			"FAST_SANDBOX_INSTANCE_GENERATION": strconv.FormatInt(config.Identity.InstanceGeneration, 10),
			"FAST_SANDBOX_ASSIGNMENT_ATTEMPT":  strconv.FormatInt(config.Identity.AssignmentAttempt, 10),
		}
		for name, value := range component.Process.Env {
			environment[name] = value
		}
		initConfig.Components = append(initConfig.Components, supervisor.Component{
			Name: component.Name, Command: component.Process.Command[0],
			Args: append([]string(nil), component.Process.Command[1:]...),
			Env:  environment, RestartPolicy: component.Process.RestartPolicy,
			Readiness: supervisor.Readiness{
				Type:    component.Process.Readiness.Type,
				Address: "127.0.0.1:" + strconv.Itoa(int(component.Endpoint.Port)),
				Path:    component.Process.Readiness.Path, Timeout: component.Process.Readiness.Timeout,
			},
		})
		result.Services = append(result.Services, ServiceEndpoint{
			Component: component.Name, Protocol: component.Endpoint.Protocol,
			Port: component.Endpoint.Port, Readiness: component.Process.Readiness,
		})
		result.Diagnostics = append(result.Diagnostics, ComponentDiagnostic{Component: component.Name, State: "Starting"})
	}

	persisted := persistedInstance{
		Version: 1,
		Identity: instanceIdentity{
			SandboxUID: config.Identity.SandboxUID, InstanceGeneration: config.Identity.InstanceGeneration,
			AssignmentAttempt: config.Identity.AssignmentAttempt,
		},
		Init: initConfig, Prepared: result,
	}
	podPath, hostPath := m.instancePaths(config.Identity.SandboxUID, config.Identity.InstanceGeneration, config.Identity.AssignmentAttempt)
	result.ConfigPodPath = podPath
	result.ConfigHostPath = hostPath
	if hasGuestComponents {
		// The guest supervisor (sandbox-init) is the only consumer of
		// infra.json. A host-process-only plan has no guest components, so
		// nothing must be delivered into the guest rootfs: the delivery
		// drivers (firecracker GuestCopy, containerd bind mounts) see an
		// empty Mounts set and skip the guest-side copy entirely.
		result.Mounts = append(result.Mounts, Mount{
			Source: hostPath, GuestSource: podPath, Destination: InstanceConfigPath,
			Options: []string{"ro", "rbind", "nosuid", "nodev", "noexec"},
		})
	}
	persisted.Prepared = result
	if _, _, err := m.writeInstance(ctx, persisted); err != nil {
		return PreparedInstance{}, err
	}
	return result, nil
}

func (m *Manager) RecoverInstance(ctx context.Context, config *fastletapi.RuntimeSandboxConfig) (PreparedInstance, error) {
	if config == nil {
		return PreparedInstance{}, errors.New("Sandbox spec is required")
	}
	file, err := os.Open(m.instanceStatePath(config.Identity.SandboxUID, config.Identity.InstanceGeneration, config.Identity.AssignmentAttempt))
	if err != nil {
		return PreparedInstance{}, err
	}
	defer file.Close()
	var persisted persistedInstance
	if err := json.NewDecoder(file).Decode(&persisted); err != nil {
		return PreparedInstance{}, err
	}
	if err := ctx.Err(); err != nil {
		return PreparedInstance{}, err
	}
	expected := instanceIdentity{
		SandboxUID: config.Identity.SandboxUID, InstanceGeneration: config.Identity.InstanceGeneration,
		AssignmentAttempt: config.Identity.AssignmentAttempt,
	}
	if persisted.Version != 1 || persisted.Identity != expected {
		return PreparedInstance{}, errors.New("persisted Infra instance identity does not match runtime")
	}
	return persisted.Prepared, nil
}

func (m *Manager) RemoveInstance(config *fastletapi.RuntimeSandboxConfig) error {
	if config == nil {
		return nil
	}
	podPath, _ := m.instancePaths(config.Identity.SandboxUID, config.Identity.InstanceGeneration, config.Identity.AssignmentAttempt)
	return os.RemoveAll(filepath.Dir(podPath))
}

func (m *Manager) RemoveSandboxInstances(sandboxUID string) error {
	if sandboxUID == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(m.config.Store.podRoot, "instances", safeSegment(sandboxUID)))
}

func (m *Manager) writeInstance(ctx context.Context, persisted persistedInstance) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	podPath, hostPath := m.instancePaths(
		persisted.Identity.SandboxUID,
		persisted.Identity.InstanceGeneration,
		persisted.Identity.AssignmentAttempt,
	)
	configStarted := time.Now()
	_, configSpan := observability.Start(ctx, "fastlet.infra.persist_config")
	err := atomicWriteJSON(podPath, persisted.Init, 0400)
	observeInstanceStage("config_persist", configStarted, err)
	observability.End(configSpan, err)
	if err != nil {
		return "", "", err
	}
	stateStarted := time.Now()
	_, stateSpan := observability.Start(ctx, "fastlet.infra.persist_state")
	err = atomicWriteJSON(
		m.instanceStatePath(
			persisted.Identity.SandboxUID,
			persisted.Identity.InstanceGeneration,
			persisted.Identity.AssignmentAttempt,
		),
		persisted,
		0400,
	)
	observeInstanceStage("state_persist", stateStarted, err)
	observability.End(stateSpan, err)
	if err != nil {
		return "", "", err
	}
	return podPath, hostPath, nil
}

func (m *Manager) instancePaths(sandboxUID string, generation, attempt int64) (string, string) {
	relative := filepath.Join(
		"instances", safeSegment(sandboxUID), fmt.Sprintf("%d-%d", generation, attempt), "infra.json",
	)
	return filepath.Join(m.config.Store.podRoot, relative), filepath.Join(m.config.Store.hostRoot, relative)
}

func (m *Manager) instanceStatePath(sandboxUID string, generation, attempt int64) string {
	podPath, _ := m.instancePaths(sandboxUID, generation, attempt)
	return filepath.Join(filepath.Dir(podPath), "state.json")
}

func atomicWriteJSON(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := json.NewEncoder(temporary).Encode(value); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, mode); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func safeSegment(value string) string {
	if value != "" && !strings.ContainsAny(value, `/\`) && value != "." && value != ".." {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("uid-%x", digest[:16])
}
