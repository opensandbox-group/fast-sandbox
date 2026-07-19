package infra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"fast-sandbox/internal/infracatalog"
	"fast-sandbox/internal/runtimecatalog"
)

type PreparedComponent struct {
	Plan     infracatalog.ComponentPlan `json:"plan"`
	Artifact *PreparedArtifact          `json:"artifact,omitempty"`
}

type PreparedPlan struct {
	infracatalog.Plan
	Supervisor *PreparedArtifact   `json:"supervisor,omitempty"`
	Components []PreparedComponent `json:"preparedComponents,omitempty"`
}

type ManagerConfig struct {
	Catalog             *infracatalog.Catalog
	RuntimeProfile      runtimecatalog.RuntimeProfile
	ProfileName         string
	ExpectedProfileHash string
	Store               *ArtifactStore
	Resolver            ArtifactResolver
	SandboxInitPath     string
}

// Manager prepares immutable profile artifacts outside the Sandbox create
// path and exposes a runtime-neutral augmentation plan to RuntimeDriver.
type Manager struct {
	mu       sync.RWMutex
	config   ManagerConfig
	plan     PreparedPlan
	prepared bool
	err      error
}

func NewManagerWithConfig(config ManagerConfig) (*Manager, error) {
	if config.Catalog == nil || config.Store == nil || config.Resolver == nil {
		return nil, errors.New("Infra catalog, artifact store, and resolver are required")
	}
	plan, err := config.Catalog.Compile(config.ProfileName, config.RuntimeProfile)
	if err != nil {
		return nil, err
	}
	if config.ExpectedProfileHash != "" && plan.ProfileHash != config.ExpectedProfileHash {
		return nil, fmt.Errorf("InfraProfile hash %s does not match expected %s", plan.ProfileHash, config.ExpectedProfileHash)
	}
	config.ProfileName = plan.ProfileName
	return &Manager{config: config, plan: PreparedPlan{Plan: plan}}, nil
}

func (m *Manager) Prepare(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prepared {
		return m.err
	}
	// A previous failure may have been a transient registry or filesystem
	// error. Keep profile admission disabled, but let the asynchronous prepare
	// loop retry the same immutable plan.
	m.err = nil
	prepared := PreparedPlan{Plan: m.plan.Plan}
	needsSupervisor := false
	for _, componentPlan := range m.plan.Plan.Components {
		component := PreparedComponent{Plan: componentPlan}
		switch componentPlan.DeliveryMode {
		case runtimecatalog.InfraDeliveryBindMount:
			artifact := componentPlan.Component.Artifact
			staged, err := m.config.Store.Stage(ctx, artifact.Digest, artifact.Executable, func() (io.ReadCloser, error) {
				return m.config.Resolver.Open(ctx, artifact)
			})
			if err != nil {
				m.err = fmt.Errorf("prepare component %s: %w", componentPlan.Component.Name, err)
				return m.err
			}
			component.Artifact = &staged
		case runtimecatalog.InfraDeliveryPreinstalled, runtimecatalog.InfraDeliveryTemplateBake:
		default:
			m.err = fmt.Errorf("delivery mode %s is not implemented by the Fastlet manager", componentPlan.DeliveryMode)
			return m.err
		}
		if componentPlan.Component.Activation.Mode != infracatalog.ActivationSystemService {
			needsSupervisor = true
		}
		prepared.Components = append(prepared.Components, component)
	}
	if needsSupervisor {
		if m.config.SandboxInitPath == "" {
			m.err = errors.New("sandbox-init path is required by the InfraProfile")
			return m.err
		}
		file, err := os.Open(m.config.SandboxInitPath)
		if err != nil {
			m.err = fmt.Errorf("open sandbox-init: %w", err)
			return m.err
		}
		staged, stageErr := m.config.Store.ImportTrusted(ctx, file, true)
		closeErr := file.Close()
		if stageErr != nil || closeErr != nil {
			m.err = errors.Join(stageErr, closeErr)
			return m.err
		}
		prepared.Supervisor = &staged
	}
	m.plan = prepared
	m.prepared = true
	return nil
}

func (m *Manager) Plan() (PreparedPlan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.prepared {
		return PreparedPlan{}, errors.New("InfraProfile artifacts are not prepared")
	}
	if m.err != nil {
		return PreparedPlan{}, m.err
	}
	return clonePreparedPlan(m.plan), nil
}

func (m *Manager) ProfileName() string { return m.plan.ProfileName }
func (m *Manager) ProfileHash() string { return m.plan.ProfileHash }

func (m *Manager) ArtifactReferences() []string {
	plan, err := m.Plan()
	if err != nil {
		return nil
	}
	references := make([]string, 0, len(plan.Components)+1)
	if plan.Supervisor != nil {
		references = append(references, plan.Supervisor.Digest)
	}
	for _, component := range plan.Components {
		if component.Artifact != nil {
			references = append(references, component.Artifact.Digest)
		}
	}
	return references
}

func clonePreparedPlan(plan PreparedPlan) PreparedPlan {
	clone := plan
	clone.Components = append([]PreparedComponent(nil), plan.Components...)
	if plan.Supervisor != nil {
		value := *plan.Supervisor
		clone.Supervisor = &value
	}
	for index := range clone.Components {
		if clone.Components[index].Artifact != nil {
			value := *clone.Components[index].Artifact
			clone.Components[index].Artifact = &value
		}
	}
	return clone
}

func DefaultStorePaths(podUID string) (string, string, error) {
	if podUID == "" {
		return "", "", errors.New("POD_UID is required for Infra artifact storage")
	}
	podRoot := "/opt/fast-sandbox/infra"
	hostRoot := filepath.Join("/var/lib/kubelet/pods", podUID, "volumes/kubernetes.io~empty-dir/infra-tools")
	return podRoot, hostRoot, nil
}
