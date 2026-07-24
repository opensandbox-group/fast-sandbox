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

	infracatalog "fast-sandbox/internal/catalog/infra"
	infracontract "fast-sandbox/internal/infra/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/sandbox/supervisor"
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

type ComponentDiagnostic = infracontract.ComponentDiagnostic

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

func (m *Manager) PrepareInstance(ctx context.Context, spec *fastletapi.SandboxSpec) (PreparedInstance, error) {
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
	initConfig := supervisor.Config{Version: supervisor.ConfigVersion, SandboxUID: spec.SandboxID}
	if plan.Supervisor != nil {
		result.WrapperRequired = true
		result.Mounts = append(result.Mounts, Mount{
			Source: plan.Supervisor.HostPath, GuestSource: plan.Supervisor.PodPath, Destination: SandboxInitContainerPath,
			Options: []string{"ro", "rbind", "nosuid", "nodev"},
		})
	}
	for _, preparedComponent := range plan.Components {
		component := preparedComponent.Plan.Component
		componentEnv := map[string]string{
			"FAST_SANDBOX_UID": spec.SandboxID, "FAST_SANDBOX_INSTANCE_GENERATION": strconv.FormatInt(spec.InstanceGeneration, 10),
			"FAST_SANDBOX_ASSIGNMENT_ATTEMPT": strconv.FormatInt(spec.AssignmentAttempt, 10),
		}
		if credential := component.InstanceInit.Credential; credential != nil {
			componentEnv[credential.EnvironmentVariable] = token
			if result.UpstreamHeaders == nil {
				result.UpstreamHeaders = make(map[string]string)
			}
			if existing, found := result.UpstreamHeaders[credential.UpstreamHeader]; found && existing != token {
				return PreparedInstance{}, fmt.Errorf("Infra credential header %s has conflicting bindings", credential.UpstreamHeader)
			}
			result.UpstreamHeaders[credential.UpstreamHeader] = token
		}
		if preparedComponent.Artifact != nil {
			result.Mounts = append(result.Mounts, Mount{
				Source: preparedComponent.Artifact.HostPath, GuestSource: preparedComponent.Artifact.PodPath, Destination: component.ContainerPath,
				Options: []string{"ro", "rbind", "nosuid", "nodev"},
			})
		}
		if component.Activation.Mode != infracatalog.ActivationSystemService {
			readiness := supervisor.Readiness{Type: infracatalog.ProbeNone}
			if len(component.Services) > 0 {
				service := component.Services[0]
				readiness = supervisor.Readiness{
					Type: service.Readiness.Type, Address: "127.0.0.1:" + strconv.Itoa(int(service.Port)),
					Path: service.Readiness.Path, Timeout: service.Readiness.Timeout, Interval: service.Readiness.Interval,
				}
			}
			initConfig.Components = append(initConfig.Components, supervisor.Component{
				Name: component.Name, Command: component.Activation.Command, Args: append([]string(nil), component.Activation.Args...),
				Env:             componentEnv,
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
	podPath, hostPath := m.instancePaths(spec.SandboxID, spec.InstanceGeneration, spec.AssignmentAttempt)
	result.ConfigPodPath = podPath
	result.ConfigHostPath = hostPath
	result.Mounts = append(result.Mounts, Mount{
		Source: hostPath, GuestSource: podPath, Destination: InstanceConfigPath,
		Options: []string{"ro", "rbind", "nosuid", "nodev", "noexec"},
	})
	// Paths are deterministic, so compile the final recovery state before the
	// first write. infra.json and state.json remain separate trust domains, but
	// each is now fsynced exactly once instead of rewriting both files after the
	// config mount is discovered.
	persisted.Prepared = result
	if _, _, err := m.writeInstance(ctx, persisted); err != nil {
		return PreparedInstance{}, err
	}
	return result, nil
}

func (m *Manager) RecoverInstance(ctx context.Context, spec *fastletapi.SandboxSpec) (PreparedInstance, error) {
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

func (m *Manager) RemoveInstance(spec *fastletapi.SandboxSpec) error {
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
