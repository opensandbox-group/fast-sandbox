// Package infra compiles the public Pool Infra Component contract into one
// deterministic, runtime-specific Fastlet plan.
package infra

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

type SourceType string

const (
	SourceOCIImage SourceType = "OCIImage"
	SourceArchive  SourceType = "Archive"
)

type ArtifactSource struct {
	Type      SourceType `json:"type"`
	Reference string     `json:"reference"`
	Digest    string     `json:"digest"`
}

type ArtifactMapping struct {
	SourcePath string `json:"sourcePath"`
	TargetPath string `json:"targetPath"`
}

type Artifact struct {
	Source   ArtifactSource    `json:"source"`
	Mappings []ArtifactMapping `json:"mappings"`
}

type RestartPolicy string

const (
	RestartNever     RestartPolicy = "Never"
	RestartOnFailure RestartPolicy = "OnFailure"
	RestartAlways    RestartPolicy = "Always"
)

type ProbeType string

const (
	ProbeHTTP ProbeType = "HTTP"
	ProbeTCP  ProbeType = "TCP"
)

type ReadinessProbe struct {
	Type    ProbeType     `json:"type"`
	Path    string        `json:"path,omitempty"`
	Timeout time.Duration `json:"timeout"`
}

type Process struct {
	Command       []string          `json:"command"`
	Env           map[string]string `json:"env,omitempty"`
	RestartPolicy RestartPolicy     `json:"restartPolicy"`
	Readiness     ReadinessProbe    `json:"readiness"`
}

type Endpoint struct {
	Protocol string `json:"protocol"`
	Port     uint32 `json:"port"`
}

type Component struct {
	Name     string                           `json:"name"`
	Artifact Artifact                         `json:"artifact"`
	Process  Process                          `json:"process"`
	Endpoint Endpoint                         `json:"endpoint"`
	Delivery runtimecatalog.InfraDeliveryMode `json:"delivery"`
}

type Plan struct {
	Revision   string      `json:"revision"`
	Components []Component `json:"components"`
}

var (
	ErrComponentsInvalid  = errors.New("Infra Components are invalid")
	ErrRuntimeUnsupported = errors.New("Infra Components are unsupported by runtime")
)

// Compile validates and normalizes one Pool definition. The resulting
// revision is the identity used by placement, Fastlet admission, assignments,
// persisted instance state, and routes.
func Compile(components []apiv1alpha2.InfraComponent, runtimeProfile runtimecatalog.RuntimeProfile) (Plan, error) {
	spec := apiv1alpha2.SandboxPoolSpec{InfraComponents: components}
	if err := spec.ValidateInfraComponents(); err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrComponentsInvalid, err)
	}
	plan := Plan{Components: make([]Component, 0, len(components))}
	for index := range components {
		if components[index].Delivery == apiv1alpha2.InfraDeliveryHostProcess {
			delivery := runtimecatalog.InfraDeliveryHostProcess
			if !supportsDelivery(runtimeProfile.InfraDeliveryModes, delivery) {
				return Plan{}, fmt.Errorf("%w: component %s requires host-process delivery unsupported by runtime %s", ErrRuntimeUnsupported, components[index].Name, runtimeProfile.Name)
			}
			plan.Components = append(plan.Components, compileComponent(components[index], Artifact{}, delivery))
			continue
		}
		var source ArtifactSource
		switch {
		case components[index].Artifact.Source.Image != nil:
			image := components[index].Artifact.Source.Image
			source = ArtifactSource{
				Type: SourceOCIImage, Reference: image.Reference,
				Digest: "sha256:" + image.Reference[len(image.Reference)-64:],
			}
		case components[index].Artifact.Source.Archive != nil:
			archive := components[index].Artifact.Source.Archive
			source = ArtifactSource{
				Type: SourceArchive, Reference: archive.URL, Digest: "sha256:" + archive.SHA256,
			}
		default:
			return Plan{}, fmt.Errorf("%w: component %s has no source", ErrComponentsInvalid, components[index].Name)
		}
		delivery, ok := selectDelivery(runtimeProfile.InfraDeliveryModes)
		if !ok {
			return Plan{}, fmt.Errorf("%w: component %s has no delivery mode for runtime %s", ErrRuntimeUnsupported, components[index].Name, runtimeProfile.Name)
		}
		mappings := make([]ArtifactMapping, 0, len(components[index].Artifact.Mappings))
		for _, mapping := range components[index].Artifact.Mappings {
			mappings = append(mappings, ArtifactMapping{
				SourcePath: mapping.SourcePath, TargetPath: mapping.TargetPath,
			})
		}
		plan.Components = append(plan.Components, compileComponent(components[index], Artifact{Source: source, Mappings: mappings}, delivery))
	}
	revision, err := Revision(plan.Components)
	if err != nil {
		return Plan{}, err
	}
	plan.Revision = revision
	return plan, nil
}

// compileComponent normalizes the runtime-neutral Process and Endpoint contract
// shared by every component regardless of delivery mode.
func compileComponent(declaration apiv1alpha2.InfraComponent, artifact Artifact, delivery runtimecatalog.InfraDeliveryMode) Component {
	restart := RestartPolicy(declaration.Process.RestartPolicy)
	if restart == "" {
		restart = RestartOnFailure
	}
	timeout := time.Duration(declaration.Process.HealthCheck.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	probe := ReadinessProbe{Timeout: timeout}
	if declaration.Process.HealthCheck.HTTPGet != nil {
		probe.Type = ProbeHTTP
		probe.Path = declaration.Process.HealthCheck.HTTPGet.Path
	} else {
		probe.Type = ProbeTCP
	}
	return Component{
		Name:     declaration.Name,
		Artifact: artifact,
		Process: Process{
			Command: append([]string(nil), declaration.Process.Command...),
			Env:     cloneStrings(declaration.Process.Env), RestartPolicy: restart, Readiness: probe,
		},
		Endpoint: Endpoint{
			Protocol: declaration.Endpoint.Protocol,
			Port:     uint32(declaration.Endpoint.Port),
		},
		Delivery: delivery,
	}
}

func supportsDelivery(runtimeModes []runtimecatalog.InfraDeliveryMode, requested runtimecatalog.InfraDeliveryMode) bool {
	for _, mode := range runtimeModes {
		if mode == requested {
			return true
		}
	}
	return false
}

func Revision(components []Component) (string, error) {
	payload, err := json.Marshal(components)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func selectDelivery(runtimeModes []runtimecatalog.InfraDeliveryMode) (runtimecatalog.InfraDeliveryMode, bool) {
	for _, preferred := range []runtimecatalog.InfraDeliveryMode{
		runtimecatalog.InfraDeliveryBindMount,
		runtimecatalog.InfraDeliveryGuestCopy,
	} {
		for _, supported := range runtimeModes {
			if preferred == supported {
				return preferred, true
			}
		}
	}
	return "", false
}

func cloneStrings(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
