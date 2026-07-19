// Package infracatalog is the shared, platform-owned source of truth for
// Sandbox Runtime Augmentation profiles. It describes artifact delivery and
// lifecycle only; component-specific Exec/File protocols are intentionally
// outside this package.
package infracatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/runtimecatalog"
)

type SourceType string

const (
	SourceEmbedded     SourceType = "Embedded"
	SourceOCIArtifact  SourceType = "OCIArtifact"
	SourceStatic       SourceType = "Static"
	SourcePreinstalled SourceType = "Preinstalled"
)

type ActivationMode string

const (
	ActivationEntrypointSupervisor ActivationMode = "EntrypointSupervisor"
	ActivationComponentBootstrap   ActivationMode = "ComponentBootstrap"
	ActivationSystemService        ActivationMode = "SystemService"
)

type RestartPolicy string

const (
	RestartNever     RestartPolicy = "Never"
	RestartOnFailure RestartPolicy = "OnFailure"
	RestartAlways    RestartPolicy = "Always"
)

type InitMode string

const (
	InitNone        InitMode = "None"
	InitEnvironment InitMode = "Environment"
	InitHTTP        InitMode = "HTTP"
)

type ProbeType string

const (
	ProbeNone ProbeType = "None"
	ProbeHTTP ProbeType = "HTTP"
	ProbeTCP  ProbeType = "TCP"
)

type Artifact struct {
	SourceType SourceType `json:"sourceType"`
	Reference  string     `json:"reference,omitempty"`
	Digest     string     `json:"digest,omitempty"`
	Executable bool       `json:"executable,omitempty"`
}

type Activation struct {
	Mode            ActivationMode `json:"mode"`
	Command         string         `json:"command"`
	Args            []string       `json:"args,omitempty"`
	StartBeforeUser bool           `json:"startBeforeUser,omitempty"`
	RestartPolicy   RestartPolicy  `json:"restartPolicy,omitempty"`
}

type InstanceInit struct {
	Mode   InitMode `json:"mode"`
	Method string   `json:"method,omitempty"`
	Path   string   `json:"path,omitempty"`
}

type ReadinessProbe struct {
	Type     ProbeType     `json:"type"`
	Path     string        `json:"path,omitempty"`
	Timeout  time.Duration `json:"timeout,omitempty"`
	Interval time.Duration `json:"interval,omitempty"`
}

type Service struct {
	Name      string         `json:"name"`
	Transport string         `json:"transport"`
	Port      uint32         `json:"port"`
	Readiness ReadinessProbe `json:"readiness"`
}

type Component struct {
	Name          string                             `json:"name"`
	Artifact      Artifact                           `json:"artifact"`
	ContainerPath string                             `json:"containerPath,omitempty"`
	DeliveryModes []runtimecatalog.InfraDeliveryMode `json:"deliveryModes"`
	Activation    Activation                         `json:"activation"`
	InstanceInit  InstanceInit                       `json:"instanceInit"`
	Services      []Service                          `json:"services,omitempty"`
	Required      bool                               `json:"required"`
	DependsOn     []string                           `json:"dependsOn,omitempty"`
}

type Profile struct {
	Name              string                    `json:"name"`
	Version           string                    `json:"version"`
	ProfileHash       string                    `json:"profileHash"`
	AllowedRuntimes   []apiv1alpha1.RuntimeName `json:"allowedRuntimes,omitempty"`
	Components        []Component               `json:"components,omitempty"`
	Configured        bool                      `json:"configured"`
	UnavailableReason string                    `json:"unavailableReason,omitempty"`
}

type ComponentPlan struct {
	Component    Component                        `json:"component"`
	DeliveryMode runtimecatalog.InfraDeliveryMode `json:"deliveryMode"`
}

type Plan struct {
	ProfileName string          `json:"profileName"`
	ProfileHash string          `json:"profileHash"`
	Components  []ComponentPlan `json:"components,omitempty"`
}

var (
	ErrProfileNotFound     = errors.New("InfraProfile not found")
	ErrProfileInvalid      = errors.New("InfraProfile is invalid")
	ErrProfileUnconfigured = errors.New("InfraProfile is not configured")
	ErrRuntimeUnsupported  = errors.New("InfraProfile is unsupported by runtime")
)

type Catalog struct {
	profiles map[string]Profile
}

func Builtin() *Catalog {
	profiles := builtinProfiles()
	for name, profile := range profiles {
		profile.ProfileHash = mustProfileHash(profile)
		profiles[name] = profile
	}
	return &Catalog{profiles: profiles}
}

func New(profiles []Profile) (*Catalog, error) {
	catalog := &Catalog{profiles: make(map[string]Profile, len(profiles))}
	for _, profile := range profiles {
		if err := Validate(profile); err != nil {
			return nil, err
		}
		if _, exists := catalog.profiles[profile.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate profile %q", ErrProfileInvalid, profile.Name)
		}
		profile.ProfileHash = mustProfileHash(profile)
		catalog.profiles[profile.Name] = cloneProfile(profile)
	}
	return catalog, nil
}

func (c *Catalog) Resolve(name string) (Profile, error) {
	if name == "" {
		name = "minimal"
	}
	profile, ok := c.profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}
	return cloneProfile(profile), nil
}

func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.profiles))
	for name := range c.profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c *Catalog) Compile(name string, runtimeProfile runtimecatalog.RuntimeProfile) (Plan, error) {
	profile, err := c.Resolve(name)
	if err != nil {
		return Plan{}, err
	}
	if err := Validate(profile); err != nil {
		return Plan{}, err
	}
	if !profile.Configured {
		reason := profile.UnavailableReason
		if reason == "" {
			reason = "platform artifact binding is missing"
		}
		return Plan{}, fmt.Errorf("%w: %s: %s", ErrProfileUnconfigured, profile.Name, reason)
	}
	if !runtimeAllowed(profile, runtimeProfile.Name) {
		return Plan{}, fmt.Errorf("%w: profile %s does not allow runtime %s", ErrRuntimeUnsupported, profile.Name, runtimeProfile.Name)
	}
	plan := Plan{ProfileName: profile.Name, ProfileHash: profile.ProfileHash}
	for _, component := range profile.Components {
		delivery, ok := selectDelivery(component.DeliveryModes, runtimeProfile.InfraDeliveryModes)
		if !ok {
			return Plan{}, fmt.Errorf("%w: component %s has no delivery mode supported by runtime %s", ErrRuntimeUnsupported, component.Name, runtimeProfile.Name)
		}
		plan.Components = append(plan.Components, ComponentPlan{Component: component, DeliveryMode: delivery})
	}
	return plan, nil
}

func Validate(profile Profile) error {
	if profile.Name == "" || profile.Version == "" {
		return fmt.Errorf("%w: name and version are required", ErrProfileInvalid)
	}
	components := make(map[string]struct{}, len(profile.Components))
	services := make(map[string]struct{})
	ports := make(map[uint32]string)
	for _, component := range profile.Components {
		if component.Name == "" || component.Activation.Mode == "" || component.Activation.Command == "" {
			return fmt.Errorf("%w: component name, activation mode, and command are required", ErrProfileInvalid)
		}
		if _, exists := components[component.Name]; exists {
			return fmt.Errorf("%w: duplicate component %q", ErrProfileInvalid, component.Name)
		}
		components[component.Name] = struct{}{}
		if len(component.DeliveryModes) == 0 {
			return fmt.Errorf("%w: component %s has no delivery mode", ErrProfileInvalid, component.Name)
		}
		if component.Artifact.SourceType != SourcePreinstalled {
			if component.Artifact.Reference == "" || !validDigest(component.Artifact.Digest) || component.ContainerPath == "" {
				return fmt.Errorf("%w: component %s requires immutable artifact reference, sha256 digest, and container path", ErrProfileInvalid, component.Name)
			}
		}
		for _, service := range component.Services {
			if service.Name == "" || service.Port == 0 || service.Port > 65535 {
				return fmt.Errorf("%w: component %s has invalid service", ErrProfileInvalid, component.Name)
			}
			if _, exists := services[service.Name]; exists {
				return fmt.Errorf("%w: duplicate service %q", ErrProfileInvalid, service.Name)
			}
			if owner, exists := ports[service.Port]; exists {
				return fmt.Errorf("%w: services %s and %s both use port %d", ErrProfileInvalid, owner, service.Name, service.Port)
			}
			services[service.Name] = struct{}{}
			ports[service.Port] = service.Name
			if service.Readiness.Type == ProbeHTTP && !strings.HasPrefix(service.Readiness.Path, "/") {
				return fmt.Errorf("%w: HTTP readiness path for %s must be absolute", ErrProfileInvalid, service.Name)
			}
		}
	}
	for _, component := range profile.Components {
		for _, dependency := range component.DependsOn {
			if _, exists := components[dependency]; !exists {
				return fmt.Errorf("%w: component %s depends on unknown component %s", ErrProfileInvalid, component.Name, dependency)
			}
		}
	}
	return validateDAG(profile.Components)
}

func ProfileHash(profile Profile) (string, error) {
	profile.ProfileHash = ""
	payload, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func mustProfileHash(profile Profile) string {
	hash, err := ProfileHash(profile)
	if err != nil {
		panic(err)
	}
	return hash
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func runtimeAllowed(profile Profile, runtimeName apiv1alpha1.RuntimeName) bool {
	if len(profile.AllowedRuntimes) == 0 {
		return true
	}
	for _, candidate := range profile.AllowedRuntimes {
		if candidate == runtimeName {
			return true
		}
	}
	return false
}

func selectDelivery(componentModes, runtimeModes []runtimecatalog.InfraDeliveryMode) (runtimecatalog.InfraDeliveryMode, bool) {
	for _, componentMode := range componentModes {
		for _, runtimeMode := range runtimeModes {
			if componentMode == runtimeMode {
				return componentMode, true
			}
		}
	}
	return "", false
}

func validateDAG(components []Component) error {
	dependencies := make(map[string][]string, len(components))
	for _, component := range components {
		dependencies[component.Name] = append([]string(nil), component.DependsOn...)
	}
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] {
			return fmt.Errorf("%w: component dependency cycle includes %s", ErrProfileInvalid, name)
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		for _, dependency := range dependencies[name] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		return nil
	}
	for name := range dependencies {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func cloneProfile(profile Profile) Profile {
	payload, _ := json.Marshal(profile)
	var clone Profile
	_ = json.Unmarshal(payload, &clone)
	return clone
}
