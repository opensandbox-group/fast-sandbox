package infra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"

	"k8s.io/klog/v2"
)

type PreparedMapping struct {
	SourcePath string `json:"sourcePath"`
	TargetPath string `json:"targetPath"`
	PodPath    string `json:"podPath"`
	HostPath   string `json:"hostPath"`
}

type PreparedComponent struct {
	Plan     infracatalog.Component `json:"plan"`
	Mappings []PreparedMapping      `json:"mappings"`
}

type PreparedPlan struct {
	infracatalog.Plan
	Supervisor *PreparedArtifact   `json:"supervisor,omitempty"`
	Tunnel     *PreparedArtifact   `json:"tunnel,omitempty"`
	Components []PreparedComponent `json:"preparedComponents,omitempty"`
}

type ManagerConfig struct {
	Plan              infracatalog.Plan
	RuntimeProfile    runtimecatalog.RuntimeProfile
	Store             *ArtifactStore
	Resolver          ArtifactResolver
	SandboxInitPath   string
	SandboxTunnelPath string
}

// Manager prepares an immutable Pool revision outside the Sandbox create
// path. Sandbox admission only consumes a plan after every source and mapping
// has been verified and staged.
type Manager struct {
	mu       sync.RWMutex
	config   ManagerConfig
	plan     PreparedPlan
	prepared bool
	err      error
}

func NewManagerWithConfig(config ManagerConfig) (*Manager, error) {
	if config.Store == nil || config.Resolver == nil {
		return nil, errors.New("Infra artifact store and resolver are required")
	}
	revision, err := infracatalog.Revision(config.Plan.Components)
	if err != nil {
		return nil, err
	}
	if config.Plan.Revision == "" {
		config.Plan.Revision = revision
	}
	if config.Plan.Revision != revision {
		return nil, fmt.Errorf("infra revision %s does not match compiled plan %s", config.Plan.Revision, revision)
	}
	return &Manager{config: config, plan: PreparedPlan{Plan: config.Plan}}, nil
}

func (m *Manager) Prepare(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prepared {
		return m.err
	}
	m.err = nil
	prepared := PreparedPlan{Plan: m.plan.Plan}
	guestComponents := false
	for _, component := range m.plan.Plan.Components {
		if component.Delivery == runtimecatalog.InfraDeliveryHostProcess {
			klog.InfoS("infra component uses host-process delivery; artifact delivery skipped",
				"component", component.Name, "delivery", component.Delivery)
			prepared.Components = append(prepared.Components, PreparedComponent{Plan: component})
			continue
		}
		guestComponents = true
		source, err := m.config.Resolver.Prepare(ctx, component.Artifact.Source, m.config.Store)
		if err != nil {
			m.err = fmt.Errorf("prepare component %s: %w", component.Name, err)
			return m.err
		}
		preparedComponent := PreparedComponent{Plan: component}
		for _, mapping := range component.Artifact.Mappings {
			resolved, err := source.Resolve(mapping.SourcePath)
			if err != nil {
				m.err = fmt.Errorf("prepare component %s mapping %s: %w", component.Name, mapping.SourcePath, err)
				return m.err
			}
			preparedComponent.Mappings = append(preparedComponent.Mappings, PreparedMapping{
				SourcePath: mapping.SourcePath,
				TargetPath: mapping.TargetPath,
				PodPath:    resolved.PodPath,
				HostPath:   resolved.HostPath,
			})
		}
		prepared.Components = append(prepared.Components, preparedComponent)
	}
	if guestComponents {
		if m.config.SandboxInitPath == "" {
			m.err = errors.New("sandbox-init path is required when Infra Components are configured")
			return m.err
		}
		supervisor, err := importTrustedFile(ctx, m.config.Store, m.config.SandboxInitPath)
		if err != nil {
			m.err = fmt.Errorf("prepare sandbox-init: %w", err)
			return m.err
		}
		prepared.Supervisor = &supervisor
	}
	if m.config.RuntimeProfile.NetworkMode == runtimecatalog.NetworkModeBoxLite {
		if m.config.SandboxTunnelPath == "" {
			m.err = errors.New("sandbox-tunnel path is required by the BoxLite runtime")
			return m.err
		}
		tunnel, err := importTrustedFile(ctx, m.config.Store, m.config.SandboxTunnelPath)
		if err != nil {
			m.err = fmt.Errorf("prepare sandbox-tunnel: %w", err)
			return m.err
		}
		prepared.Tunnel = &tunnel
	}
	m.plan = prepared
	m.prepared = true
	return nil
}

func importTrustedFile(ctx context.Context, store *ArtifactStore, path string) (PreparedArtifact, error) {
	file, err := os.Open(path)
	if err != nil {
		return PreparedArtifact{}, err
	}
	defer file.Close()
	return store.ImportTrusted(ctx, file, true)
}

func (m *Manager) Plan() (PreparedPlan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.prepared {
		return PreparedPlan{}, errors.New("Infra Components are not prepared")
	}
	if m.err != nil {
		return PreparedPlan{}, m.err
	}
	return clonePreparedPlan(m.plan), nil
}

func (m *Manager) Revision() string { return m.plan.Revision }

func (m *Manager) ArtifactReferences() []string {
	plan, err := m.Plan()
	if err != nil {
		return nil
	}
	references := make([]string, 0, len(plan.Components)+2)
	if plan.Supervisor != nil {
		references = append(references, plan.Supervisor.Digest)
	}
	if plan.Tunnel != nil {
		references = append(references, plan.Tunnel.Digest)
	}
	for _, component := range plan.Components {
		if component.Plan.Delivery == runtimecatalog.InfraDeliveryHostProcess {
			continue
		}
		references = append(references, component.Plan.Artifact.Source.Digest)
	}
	return references
}

func clonePreparedPlan(plan PreparedPlan) PreparedPlan {
	clone := plan
	clone.Components = make([]PreparedComponent, len(plan.Components))
	for index := range plan.Components {
		clone.Components[index] = plan.Components[index]
		clone.Components[index].Mappings = append([]PreparedMapping(nil), plan.Components[index].Mappings...)
	}
	if plan.Supervisor != nil {
		value := *plan.Supervisor
		clone.Supervisor = &value
	}
	if plan.Tunnel != nil {
		value := *plan.Tunnel
		clone.Tunnel = &value
	}
	return clone
}

func StorePaths(podUID, kubeletRoot string) (string, string, error) {
	if podUID == "" {
		return "", "", errors.New("POD_UID is required for Infra artifact storage")
	}
	if !filepath.IsAbs(kubeletRoot) {
		return "", "", errors.New("kubelet root must be an absolute path")
	}
	podRoot := "/opt/fast-sandbox/infra"
	hostRoot := filepath.Join(kubeletRoot, "pods", podUID, "volumes/kubernetes.io~empty-dir/infra-tools")
	return podRoot, hostRoot, nil
}
