package infra

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/infracatalog"
	"fast-sandbox/internal/sandboxinit"
)

const (
	SandboxInitContainerPath   = "/.fast/bin/sandbox-init"
	SandboxTunnelContainerPath = "/.fast/bin/sandbox-tunnel"
	InstanceConfigPath         = "/.fast/run/infra.json"
	UpstreamTokenHeader        = "X-Fast-Sandbox-Infra-Token"
)

type Mount struct {
	Source      string   `json:"source"`
	GuestSource string   `json:"guestSource,omitempty"`
	Destination string   `json:"destination"`
	Options     []string `json:"options"`
}

type ServiceEndpoint struct {
	Component string                      `json:"component"`
	Name      string                      `json:"name"`
	Port      uint32                      `json:"port"`
	Readiness infracatalog.ReadinessProbe `json:"readiness"`
	Required  bool                        `json:"required"`
	Init      infracatalog.InstanceInit   `json:"init"`
}

type PreparedInstance struct {
	SandboxUID      string                `json:"sandboxUid"`
	ConfigPodPath   string                `json:"configPodPath"`
	ConfigHostPath  string                `json:"configHostPath"`
	Mounts          []Mount               `json:"mounts"`
	WrapperRequired bool                  `json:"wrapperRequired"`
	Services        []ServiceEndpoint     `json:"services,omitempty"`
	UpstreamHeaders map[string]string     `json:"upstreamHeaders,omitempty"`
	Diagnostics     []ComponentDiagnostic `json:"diagnostics,omitempty"`
}

type ComponentDiagnostic struct {
	Component string `json:"component"`
	Service   string `json:"service"`
	Required  bool   `json:"required"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
}

type persistedInstance struct {
	Version  int                `json:"version"`
	Identity instanceIdentity   `json:"identity"`
	Init     sandboxinit.Config `json:"sandboxInit"`
	Prepared PreparedInstance   `json:"prepared"`
}

type instanceIdentity struct {
	SandboxUID         string `json:"sandboxUid"`
	InstanceGeneration int64  `json:"instanceGeneration"`
	AssignmentAttempt  int64  `json:"assignmentAttempt"`
}

func (m *Manager) PrepareInstance(ctx context.Context, spec *api.SandboxSpec) (PreparedInstance, error) {
	if spec == nil || spec.SandboxID == "" || spec.InstanceGeneration <= 0 || spec.AssignmentAttempt <= 0 {
		return PreparedInstance{}, errors.New("Sandbox UID, instance generation, and assignment attempt are required for Infra init")
	}
	plan, err := m.Plan()
	if err != nil {
		return PreparedInstance{}, err
	}
	if plan.ProfileName != spec.InfraProfile || plan.ProfileHash != spec.InfraProfileHash {
		return PreparedInstance{}, fmt.Errorf("Sandbox InfraProfile identity does not match prepared plan")
	}
	if len(plan.Components) == 0 && plan.Tunnel == nil {
		return PreparedInstance{SandboxUID: spec.SandboxID}, nil
	}
	result := PreparedInstance{SandboxUID: spec.SandboxID}
	if plan.Tunnel != nil {
		result.Mounts = append(result.Mounts, Mount{
			Source: plan.Tunnel.HostPath, GuestSource: plan.Tunnel.PodPath, Destination: SandboxTunnelContainerPath,
			Options: []string{"ro", "nosuid", "nodev"},
		})
	}
	if len(plan.Components) == 0 {
		return result, nil
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return PreparedInstance{}, fmt.Errorf("generate Infra instance token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	result.UpstreamHeaders = map[string]string{UpstreamTokenHeader: token}
	initConfig := sandboxinit.Config{Version: sandboxinit.ConfigVersion, SandboxUID: spec.SandboxID}
	if plan.Supervisor != nil {
		result.WrapperRequired = true
		result.Mounts = append(result.Mounts, Mount{
			Source: plan.Supervisor.HostPath, GuestSource: plan.Supervisor.PodPath, Destination: SandboxInitContainerPath,
			Options: []string{"ro", "rbind", "nosuid", "nodev"},
		})
	}
	for _, preparedComponent := range plan.Components {
		component := preparedComponent.Plan.Component
		if preparedComponent.Artifact != nil {
			result.Mounts = append(result.Mounts, Mount{
				Source: preparedComponent.Artifact.HostPath, GuestSource: preparedComponent.Artifact.PodPath, Destination: component.ContainerPath,
				Options: []string{"ro", "rbind", "nosuid", "nodev"},
			})
		}
		if component.Activation.Mode != infracatalog.ActivationSystemService {
			readiness := sandboxinit.Readiness{Type: infracatalog.ProbeNone}
			if len(component.Services) > 0 {
				service := component.Services[0]
				readiness = sandboxinit.Readiness{
					Type: service.Readiness.Type, Address: "127.0.0.1:" + strconv.Itoa(int(service.Port)),
					Path: service.Readiness.Path, Timeout: service.Readiness.Timeout, Interval: service.Readiness.Interval,
				}
			}
			initConfig.Components = append(initConfig.Components, sandboxinit.Component{
				Name: component.Name, Command: component.Activation.Command, Args: append([]string(nil), component.Activation.Args...),
				Env: map[string]string{
					"FAST_SANDBOX_UID": spec.SandboxID, "FAST_SANDBOX_INSTANCE_GENERATION": strconv.FormatInt(spec.InstanceGeneration, 10),
					"FAST_SANDBOX_ASSIGNMENT_ATTEMPT": strconv.FormatInt(spec.AssignmentAttempt, 10), "FAST_SANDBOX_INTERNAL_TOKEN": token,
				},
				StartBeforeUser: component.Activation.StartBeforeUser, RestartPolicy: component.Activation.RestartPolicy, Readiness: readiness,
				Required: component.Required, DependsOn: append([]string(nil), component.DependsOn...),
			})
		}
		for _, service := range component.Services {
			result.Services = append(result.Services, ServiceEndpoint{
				Component: component.Name, Name: service.Name, Port: service.Port, Readiness: service.Readiness,
				Required: component.Required, Init: component.InstanceInit,
			})
		}
	}
	persisted := persistedInstance{
		Version:  1,
		Identity: instanceIdentity{SandboxUID: spec.SandboxID, InstanceGeneration: spec.InstanceGeneration, AssignmentAttempt: spec.AssignmentAttempt},
		Init:     initConfig, Prepared: result,
	}
	podPath, hostPath, err := m.writeInstance(ctx, persisted)
	if err != nil {
		return PreparedInstance{}, err
	}
	result.ConfigPodPath = podPath
	result.ConfigHostPath = hostPath
	result.Mounts = append(result.Mounts, Mount{
		Source: hostPath, GuestSource: podPath, Destination: InstanceConfigPath,
		Options: []string{"ro", "rbind", "nosuid", "nodev", "noexec"},
	})
	// Persist final paths and mounts as the recovery source.
	persisted.Prepared = result
	if _, _, err := m.writeInstance(ctx, persisted); err != nil {
		return PreparedInstance{}, err
	}
	return result, nil
}

func (m *Manager) RecoverInstance(ctx context.Context, spec *api.SandboxSpec) (PreparedInstance, error) {
	if spec == nil {
		return PreparedInstance{}, errors.New("Sandbox spec is required")
	}
	statePath := m.instanceStatePath(spec.SandboxID, spec.InstanceGeneration, spec.AssignmentAttempt)
	file, err := os.Open(statePath)
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
	if persisted.Version != 1 || persisted.Identity != (instanceIdentity{
		SandboxUID: spec.SandboxID, InstanceGeneration: spec.InstanceGeneration, AssignmentAttempt: spec.AssignmentAttempt,
	}) {
		return PreparedInstance{}, errors.New("persisted Infra instance identity does not match runtime")
	}
	return persisted.Prepared, nil
}

func (m *Manager) RemoveInstance(spec *api.SandboxSpec) error {
	if spec == nil {
		return nil
	}
	podPath, _ := m.instancePaths(spec.SandboxID, spec.InstanceGeneration, spec.AssignmentAttempt)
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
	podPath, hostPath := m.instancePaths(persisted.Identity.SandboxUID, persisted.Identity.InstanceGeneration, persisted.Identity.AssignmentAttempt)
	if err := atomicWriteJSON(podPath, persisted.Init, 0400); err != nil {
		return "", "", err
	}
	if err := atomicWriteJSON(m.instanceStatePath(persisted.Identity.SandboxUID, persisted.Identity.InstanceGeneration, persisted.Identity.AssignmentAttempt), persisted, 0400); err != nil {
		return "", "", err
	}
	return podPath, hostPath, nil
}

func (m *Manager) instancePaths(sandboxUID string, generation, attempt int64) (string, string) {
	segment := safeSegment(sandboxUID)
	relative := filepath.Join("instances", segment, fmt.Sprintf("%d-%d", generation, attempt), "infra.json")
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
	if value != "" && !strings.ContainsAny(value, `/\\`) && value != "." && value != ".." {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("uid-%x", digest[:16])
}
