package runtimeenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	actionapi "fast-sandbox/internal/protocol/action"
)

var ErrEnvironmentNotFound = errors.New("runtime environment not found")

func Resolve(catalog *runtimecatalog.Catalog, config Config, runtimeName apiv1alpha2.RuntimeName) (ResolvedRuntimePlan, error) {
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	profile, err := catalog.Resolve(runtimeName)
	if err != nil {
		return ResolvedRuntimePlan{}, err
	}
	environmentName, environment, binding, err := environmentForRuntime(config, profile.Name, profile.Driver)
	if err != nil {
		return ResolvedRuntimePlan{}, err
	}
	if err := applyEnvironment(&profile, environment, binding); err != nil {
		return ResolvedRuntimePlan{}, fmt.Errorf("resolve runtime %s in environment %s: %w", profile.Name, environmentName, err)
	}
	actionsAPIVersion := config.ActionsAPIVersion
	if actionsAPIVersion == "" {
		actionsAPIVersion = actionapi.APIVersion
	}
	if actionsAPIVersion != actionapi.APIVersion && actionsAPIVersion != actionapi.APIVersionV2 {
		return ResolvedRuntimePlan{}, fmt.Errorf("unsupported Actions apiVersion %q", actionsAPIVersion)
	}
	if actionsAPIVersion == actionapi.APIVersionV2 && profile.Name != apiv1alpha2.RuntimeFirecracker {
		return ResolvedRuntimePlan{}, fmt.Errorf("Actions apiVersion %q is supported only by runtime %s", actionsAPIVersion, apiv1alpha2.RuntimeFirecracker)
	}
	plan := ResolvedRuntimePlan{
		Version: PlanVersion, Environment: environmentName, ActionsAPIVersion: actionsAPIVersion, Profile: profile,
		Containerd: resolvedContainerdEnvironment(environment.Containerd, profile), Kubelet: environment.Kubelet,
	}
	if err := stampRevision(&plan); err != nil {
		return ResolvedRuntimePlan{}, err
	}
	return plan, nil
}

func ResolveDefault(catalog *runtimecatalog.Catalog, runtimeName apiv1alpha2.RuntimeName) (ResolvedRuntimePlan, error) {
	return Resolve(catalog, DefaultConfig(), runtimeName)
}

func DecodePlan(reader io.Reader) (ResolvedRuntimePlan, error) {
	var plan ResolvedRuntimePlan
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return ResolvedRuntimePlan{}, fmt.Errorf("decode runtime plan: %w", err)
	}
	expected := plan.Revision
	expectedProfileHash := plan.Profile.ProfileHash
	if expected == "" || plan.Version != PlanVersion {
		return ResolvedRuntimePlan{}, errors.New("runtime plan version and revision are required")
	}
	if plan.ActionsAPIVersion != "" && plan.ActionsAPIVersion != actionapi.APIVersion && plan.ActionsAPIVersion != actionapi.APIVersionV2 {
		return ResolvedRuntimePlan{}, fmt.Errorf("unsupported Actions apiVersion in runtime plan %q", plan.ActionsAPIVersion)
	}
	if err := stampRevision(&plan); err != nil {
		return ResolvedRuntimePlan{}, err
	}
	if plan.Revision != expected {
		return ResolvedRuntimePlan{}, fmt.Errorf("runtime plan revision %s does not match payload %s", expected, plan.Revision)
	}
	if plan.Profile.ProfileHash != expectedProfileHash {
		return ResolvedRuntimePlan{}, fmt.Errorf("runtime profile hash %s does not match payload %s", expectedProfileHash, plan.Profile.ProfileHash)
	}
	return plan, nil
}

func environmentForRuntime(config Config, runtimeName apiv1alpha2.RuntimeName, driver runtimecatalog.DriverKind) (string, NodeRuntimeEnvironment, RuntimeBinding, error) {
	for name, environment := range config.Environments {
		if binding, exists := environment.Runtimes[runtimeName]; exists {
			return name, environment, binding, nil
		}
	}
	if driver != runtimecatalog.DriverKindContainerd {
		if environment, exists := config.Environments[DefaultEnvironment]; exists {
			return DefaultEnvironment, environment, RuntimeBinding{}, nil
		}
	}
	return "", NodeRuntimeEnvironment{}, RuntimeBinding{}, fmt.Errorf("%w for runtime %s", ErrEnvironmentNotFound, runtimeName)
}

func applyEnvironment(profile *runtimecatalog.RuntimeProfile, environment NodeRuntimeEnvironment, binding RuntimeBinding) error {
	if err := mergeStringMap(profile.Deployment.NodeSelector, environment.NodeSelector); err != nil {
		return err
	}
	if profile.Deployment.NodeSelector == nil && len(environment.NodeSelector) > 0 {
		profile.Deployment.NodeSelector = make(map[string]string, len(environment.NodeSelector))
		for key, value := range environment.NodeSelector {
			profile.Deployment.NodeSelector[key] = value
		}
	}
	if len(binding.FastletOverhead) > 0 {
		profile.Deployment.Overhead = binding.FastletOverhead.DeepCopy()
	}
	paths := append([]runtimecatalog.HostPathRequirement(nil), profile.Deployment.HostPaths...)
	paths = append(paths, environment.HostPaths...)
	if profile.Firecracker != nil {
		applyFirecrackerBinding(profile.Firecracker, binding.Firecracker)
		paths = replaceFirecrackerHostPaths(paths, profile.Firecracker)
	}
	if profile.Driver == runtimecatalog.DriverKindContainerd {
		if profile.Containerd == nil {
			return errors.New("containerd runtime definition has no configuration")
		}
		profile.Containerd.Namespace = environment.Containerd.Namespace
		profile.Containerd.Snapshotter = environment.Containerd.DefaultSnapshotter
		if binding.Snapshotter != "" {
			profile.Containerd.Snapshotter = binding.Snapshotter
		}
		if binding.Handler != "" {
			profile.Containerd.Handler = binding.Handler
		}
		if binding.RuntimePath != "" {
			profile.Containerd.RuntimePath = binding.RuntimePath
		}
		if binding.ConfigPath != "" {
			profile.Containerd.ConfigPath = binding.ConfigPath
		}
		if binding.OptionsType != "" {
			profile.Containerd.OptionsType = binding.OptionsType
		}
		if binding.NeedsTTY != nil {
			profile.Containerd.NeedsTTY = *binding.NeedsTTY
		}
		if profile.Containerd.Handler == "" {
			return errors.New("containerd handler is required")
		}
		paths = append(paths,
			runtimecatalog.HostPathRequirement{Name: "containerd-run", HostPath: filepath.Dir(environment.Containerd.Socket), MountPath: filepath.Dir(environment.Containerd.Socket), Type: corev1.HostPathDirectory},
			runtimecatalog.HostPathRequirement{Name: "containerd-root", HostPath: environment.Containerd.Root, MountPath: environment.Containerd.Root, Type: corev1.HostPathDirectory},
		)
	}
	paths = append(paths, binding.HostPaths...)
	if profile.Deployment.RequiresKVM {
		paths = append(paths, runtimecatalog.HostPathRequirement{Name: "dev-kvm", HostPath: "/dev/kvm", MountPath: "/dev/kvm", Type: corev1.HostPathCharDev})
	}
	merged, err := mergeHostPaths(paths)
	if err != nil {
		return err
	}
	profile.Deployment.HostPaths = merged
	return nil
}

// applyFirecrackerBinding overrides the builtin Firecracker configuration
// with the environment's installation values; empty override fields keep the
// builtin default. A nil binding is a no-op.
func applyFirecrackerBinding(config *runtimecatalog.FirecrackerConfig, binding *FirecrackerBinding) {
	if config == nil || binding == nil {
		return
	}
	if binding.BinaryPath != "" {
		config.BinaryPath = binding.BinaryPath
	}
	if binding.JailerPath != "" {
		config.JailerPath = binding.JailerPath
	}
	if binding.KernelPath != "" {
		config.KernelPath = binding.KernelPath
	}
	if binding.RootfsPath != "" {
		config.RootfsPath = binding.RootfsPath
	}
	if binding.StateRoot != "" {
		config.StateRoot = binding.StateRoot
	}
}

// replaceFirecrackerHostPaths drops the builtin firecracker-* host path
// requirements from values and appends the ones derived from the resolved
// configuration, so the fastlet pod mounts exactly the installed paths
// (binary, jailer, rootfs and state root; the kernel only
// when pinned).
func replaceFirecrackerHostPaths(values []runtimecatalog.HostPathRequirement, config *runtimecatalog.FirecrackerConfig) []runtimecatalog.HostPathRequirement {
	kept := values[:0]
	for _, requirement := range values {
		if !strings.HasPrefix(requirement.Name, "firecracker-") {
			kept = append(kept, requirement)
		}
	}
	kept = append(kept,
		runtimecatalog.HostPathRequirement{Name: "firecracker-bin", HostPath: config.BinaryPath, MountPath: config.BinaryPath, Type: corev1.HostPathFile, ReadOnly: true},
		runtimecatalog.HostPathRequirement{Name: "firecracker-rootfs", HostPath: config.RootfsPath, MountPath: config.RootfsPath, Type: corev1.HostPathDirectoryOrCreate},
		runtimecatalog.HostPathRequirement{Name: "firecracker-state", HostPath: config.StateRoot, MountPath: config.StateRoot, Type: corev1.HostPathDirectoryOrCreate},
	)
	// The kernel mounts only when an operator pins kernelPath (#84).
	if config.KernelPath != "" {
		kept = append(kept, runtimecatalog.HostPathRequirement{Name: "firecracker-kernel", HostPath: config.KernelPath, MountPath: config.KernelPath, Type: corev1.HostPathFile, ReadOnly: true})
	}
	if config.JailerPath != "" {
		kept = append(kept, runtimecatalog.HostPathRequirement{Name: "firecracker-jailer", HostPath: config.JailerPath, MountPath: config.JailerPath, Type: corev1.HostPathFile, ReadOnly: true})
	}
	return kept
}

func resolvedContainerdEnvironment(environment ContainerdEnvironment, profile runtimecatalog.RuntimeProfile) ResolvedContainerdEnvironment {
	resolved := ResolvedContainerdEnvironment{
		Socket: environment.Socket, Namespace: environment.Namespace,
		Snapshotter: environment.DefaultSnapshotter, Root: environment.Root,
	}
	if profile.Containerd != nil && profile.Containerd.Snapshotter != "" {
		resolved.Snapshotter = profile.Containerd.Snapshotter
	}
	return resolved
}

func mergeStringMap(existing map[string]string, incoming map[string]string) error {
	for key, value := range incoming {
		if current, exists := existing[key]; exists && current != value {
			return fmt.Errorf("nodeSelector %s conflicts between runtime and environment", key)
		}
		if existing != nil {
			existing[key] = value
		}
	}
	return nil
}

func mergeHostPaths(values []runtimecatalog.HostPathRequirement) ([]runtimecatalog.HostPathRequirement, error) {
	byName := make(map[string]runtimecatalog.HostPathRequirement)
	for _, value := range values {
		if current, exists := byName[value.Name]; exists {
			if current != value {
				return nil, fmt.Errorf("hostPath %s has conflicting definitions", value.Name)
			}
			continue
		}
		byName[value.Name] = value
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]runtimecatalog.HostPathRequirement, 0, len(names))
	for _, name := range names {
		result = append(result, byName[name])
	}
	return result, nil
}

func stampRevision(plan *ResolvedRuntimePlan) error {
	plan.Revision = ""
	plan.Profile.ProfileHash = ""
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	value := hex.EncodeToString(digest[:])
	plan.Revision = "sha256:" + value
	plan.Profile.ProfileHash = value
	return nil
}
