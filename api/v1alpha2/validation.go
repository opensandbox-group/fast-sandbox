package v1alpha2

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

const InitialInstanceGeneration int64 = 1

const (
	MaxActionBindingInputBytes        = 64 << 10
	MaxSandboxActionBindingInputBytes = 256 << 10
)

var (
	ErrRuntimeImmutable       = errors.New("spec.runtime is immutable")
	ErrResourcesImmutable     = errors.New("spec.sandboxResources is immutable")
	ErrInfraComponentsInvalid = errors.New("spec.infraComponents is invalid")
	ErrActionHandlersInvalid  = errors.New("spec.actionHandlers is invalid")
	ErrActionHandlersRemoved  = errors.New("existing spec.actionHandlers names cannot be removed or renamed")
	ErrActionBindingsInvalid  = errors.New("spec.actionBindings is invalid")
	ErrRuntimeOwnerResources  = errors.New("ResourceAware runtime owner resources are derived")
)

// ValidateResourceContract enforces the mode-specific source of truth. Legacy
// Pools retain explicit aggregate Fastlet resources; ResourceAware Pools must
// derive the runtime-owner CPU and memory envelope from Sandbox resources.
func (s *SandboxPoolSpec) ValidateResourceContract() error {
	if s == nil {
		return fmt.Errorf("%w: pool spec is required", ErrRuntimeOwnerResources)
	}
	if err := ValidateSandboxResourceProfile(s.SandboxResources); err != nil {
		return err
	}
	if s.SandboxResources.ResourceMode() != SandboxResourceModeResourceAware {
		return nil
	}
	if len(s.FastletTemplate.Spec.Containers) == 0 {
		return fmt.Errorf("%w: fastletTemplate.spec.containers must contain the fastlet container", ErrRuntimeOwnerResources)
	}
	owner := s.FastletTemplate.Spec.Containers[0]
	for _, resources := range []struct {
		name   string
		values corev1.ResourceList
	}{
		{name: "requests", values: owner.Resources.Requests},
		{name: "limits", values: owner.Resources.Limits},
	} {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if _, configured := resources.values[name]; configured {
				return fmt.Errorf("%w: container %q must not set resources.%s.%s", ErrRuntimeOwnerResources, owner.Name, resources.name, name)
			}
		}
	}
	return nil
}

var reservedActionHandlerPorts = map[int32]string{
	5758: "Fastlet control",
	5780: "Fastlet Proxy data",
	9093: "Fastlet Proxy metrics",
}

// ValidateActionBindings verifies the ordered per-Sandbox inputs against the
// Handler names declared by its Pool.
func (s *SandboxSpec) ValidateActionBindings(handlers []ActionHandler) error {
	if s == nil {
		return fmt.Errorf("%w: Sandbox spec is required", ErrActionBindingsInvalid)
	}
	if len(s.ActionBindings) > 16 {
		return fmt.Errorf("%w: at most 16 Action Bindings are allowed", ErrActionBindingsInvalid)
	}
	declared := make(map[string]struct{}, len(handlers))
	for _, handler := range handlers {
		declared[handler.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(s.ActionBindings))
	totalInputBytes := 0
	for index, binding := range s.ActionBindings {
		if _, found := declared[binding.Handler]; !found {
			return fmt.Errorf("%w: Handler %q is not declared by the Pool", ErrActionBindingsInvalid, binding.Handler)
		}
		if _, found := seen[binding.Handler]; found {
			return fmt.Errorf("%w: duplicate Handler %q at index %d", ErrActionBindingsInvalid, binding.Handler, index)
		}
		seen[binding.Handler] = struct{}{}
		if len(binding.Input) > MaxActionBindingInputBytes {
			return fmt.Errorf("%w: Handler %q input exceeds %d bytes", ErrActionBindingsInvalid, binding.Handler, MaxActionBindingInputBytes)
		}
		totalInputBytes += len(binding.Input)
		if totalInputBytes > MaxSandboxActionBindingInputBytes {
			return fmt.Errorf("%w: Action Binding inputs exceed %d bytes", ErrActionBindingsInvalid, MaxSandboxActionBindingInputBytes)
		}
	}
	return nil
}

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// IsRuntimeName reports whether name identifies a built-in runtime profile.
func IsRuntimeName(name RuntimeName) bool {
	switch name {
	case RuntimeContainer, RuntimeGVisor, RuntimeKataQemu, RuntimeKataClh, RuntimeKataFc, RuntimeKataDragonball, RuntimeFirecracker:
		return true
	default:
		return false
	}
}

// ValidateActionHandlers verifies Handler names, transport oneOf constraints,
// exec command templates, and lifecycle Hook subscriptions.
func (s *SandboxPoolSpec) ValidateActionHandlers() error { //nolint:gocognit // cross-field validation across HTTP and exec ActionHandler modes
	if s == nil {
		return fmt.Errorf("%w: pool spec is required", ErrActionHandlersInvalid)
	}
	if len(s.ActionHandlers) > 16 {
		return fmt.Errorf("%w: at most 16 Action Handlers are allowed", ErrActionHandlersInvalid)
	}
	names := make(map[string]struct{}, len(s.ActionHandlers))
	ports := make(map[int32]string, len(s.ActionHandlers))
	supportedHooks := map[LifecycleHook]struct{}{
		LifecycleHookRuntimeReady: {}, LifecycleHookDataPlaneReady: {},
	}
	for index := range s.ActionHandlers {
		handler := &s.ActionHandlers[index]
		if problems := k8svalidation.IsDNS1123Label(handler.Name); len(problems) != 0 || strings.HasPrefix(handler.Name, "fast-sandbox-") {
			return fmt.Errorf("%w: Handler %d has invalid or reserved name %q", ErrActionHandlersInvalid, index, handler.Name)
		}
		if _, found := names[handler.Name]; found {
			return fmt.Errorf("%w: duplicate Handler name %q", ErrActionHandlersInvalid, handler.Name)
		}
		names[handler.Name] = struct{}{}
		hasHTTP := handler.TargetHTTPPort != 0
		hasExec := handler.Exec != nil
		if hasHTTP == hasExec {
			return fmt.Errorf("%w: Handler %s must declare exactly one of targetHTTPPort or exec", ErrActionHandlersInvalid, handler.Name)
		}
		if hasHTTP {
			if handler.TargetHTTPPort < 1 || handler.TargetHTTPPort > 65535 {
				return fmt.Errorf("%w: Handler %s targetHTTPPort is invalid", ErrActionHandlersInvalid, handler.Name)
			}
			if owner, reserved := reservedActionHandlerPorts[handler.TargetHTTPPort]; reserved {
				return fmt.Errorf("%w: Handler %s targetHTTPPort %d is reserved by %s", ErrActionHandlersInvalid, handler.Name, handler.TargetHTTPPort, owner)
			}
			if owner, found := ports[handler.TargetHTTPPort]; found {
				return fmt.Errorf("%w: Handlers %s and %s use duplicate targetHTTPPort %d", ErrActionHandlersInvalid, owner, handler.Name, handler.TargetHTTPPort)
			}
			ports[handler.TargetHTTPPort] = handler.Name
		} else {
			if err := validateExecActionHandler(handler.Name, handler.Exec); err != nil {
				return err
			}
		}
		hooks := make(map[LifecycleHook]struct{}, len(handler.Hooks))
		for _, hook := range handler.Hooks {
			if _, supported := supportedHooks[hook]; !supported {
				return fmt.Errorf("%w: Handler %s declares unsupported lifecycle Hook %q", ErrActionHandlersInvalid, handler.Name, hook)
			}
			if _, duplicate := hooks[hook]; duplicate {
				return fmt.Errorf("%w: Handler %s declares duplicate lifecycle Hook %q", ErrActionHandlersInvalid, handler.Name, hook)
			}
			hooks[hook] = struct{}{}
		}
	}
	return nil
}

// execTemplateSample matches the platform-trusted template context rendered by
// Fastlet (never exposing binding.input) so invalid field references fail Pool
// validation before reaching any node.
var execTemplateSample = struct {
	Sandbox struct {
		UID       string
		Name      string
		Namespace string
	}
	Revision struct {
		InstanceGeneration int64
		SpecGeneration     int64
		RuntimeInstanceID  string
		AttachmentID       string
		RouteGeneration    int64
	}
	Attachment struct {
		Restored  bool
		Recovered bool
		Network   struct {
			IP          string
			Gateway     string
			NetNSPath   string
			PrivateCIDR string
			HostVeth    string
		}
	}
}{}

func validateExecActionHandler(handlerName string, cfg *ExecActionHandler) error {
	if len(cfg.Command) == 0 || len(cfg.Command) > 64 {
		return fmt.Errorf("%w: Handler %s exec.command must contain 1..64 arguments", ErrActionHandlersInvalid, handlerName)
	}
	if strings.TrimSpace(cfg.Command[0]) == "" {
		return fmt.Errorf("%w: Handler %s exec.command[0] must be non-empty", ErrActionHandlersInvalid, handlerName)
	}
	if err := validateExecArgTemplates(handlerName, "command", cfg.Command); err != nil {
		return err
	}
	if len(cfg.CleanupCommand) > 64 {
		return fmt.Errorf("%w: Handler %s exec.cleanupCommand allows at most 64 arguments", ErrActionHandlersInvalid, handlerName)
	}
	if len(cfg.CleanupCommand) > 0 {
		if strings.TrimSpace(cfg.CleanupCommand[0]) == "" {
			return fmt.Errorf("%w: Handler %s exec.cleanupCommand[0] must be non-empty", ErrActionHandlersInvalid, handlerName)
		}
		if err := validateExecArgTemplates(handlerName, "cleanupCommand", cfg.CleanupCommand); err != nil {
			return err
		}
	}
	if len(cfg.Env) > 64 {
		return fmt.Errorf("%w: Handler %s exec.env allows at most 64 variables", ErrActionHandlersInvalid, handlerName)
	}
	envNames := make(map[string]struct{}, len(cfg.Env))
	for _, envVar := range cfg.Env {
		if envVar.Name == "" || strings.ContainsAny(envVar.Name, "=\x00") {
			return fmt.Errorf("%w: Handler %s exec.env has invalid variable name %q", ErrActionHandlersInvalid, handlerName, envVar.Name)
		}
		if _, dup := envNames[envVar.Name]; dup {
			return fmt.Errorf("%w: Handler %s exec.env declares duplicate variable %q", ErrActionHandlersInvalid, handlerName, envVar.Name)
		}
		envNames[envVar.Name] = struct{}{}
	}
	switch cfg.Stdin {
	case "", ExecActionStdinNone, ExecActionStdinBindingInput, ExecActionStdinEnvelope:
	default:
		return fmt.Errorf("%w: Handler %s exec.stdin %q is unsupported", ErrActionHandlersInvalid, handlerName, cfg.Stdin)
	}
	if cfg.TimeoutSeconds < 0 || cfg.TimeoutSeconds > 300 {
		return fmt.Errorf("%w: Handler %s exec.timeoutSeconds must be between 1 and 300", ErrActionHandlersInvalid, handlerName)
	}
	if len(cfg.NonRetryableExitCodes) > 32 {
		return fmt.Errorf("%w: Handler %s exec.nonRetryableExitCodes allows at most 32 entries", ErrActionHandlersInvalid, handlerName)
	}
	for _, code := range cfg.NonRetryableExitCodes {
		if code < 1 || code > 255 {
			return fmt.Errorf("%w: Handler %s exec.nonRetryableExitCodes entry %d must be in 1..255", ErrActionHandlersInvalid, handlerName, code)
		}
	}
	if cfg.MaxRetries < 0 || cfg.MaxRetries > 100 {
		return fmt.Errorf("%w: Handler %s exec.maxRetries must be between 0 and 100", ErrActionHandlersInvalid, handlerName)
	}
	switch cfg.FailurePolicy {
	case "", ExecActionFailurePolicyRetry, ExecActionFailurePolicyFailSandbox:
	default:
		return fmt.Errorf("%w: Handler %s exec.failurePolicy %q is unsupported", ErrActionHandlersInvalid, handlerName, cfg.FailurePolicy)
	}
	return nil
}

func validateExecArgTemplates(handlerName, field string, args []string) error {
	for index, raw := range args {
		parsed, err := template.New(fmt.Sprintf("%s[%d]", field, index)).Option("missingkey=error").Parse(raw)
		if err != nil {
			return fmt.Errorf("%w: Handler %s exec.%s[%d] template parse failed: %w", ErrActionHandlersInvalid, handlerName, field, index, err)
		}
		if err := parsed.Execute(io.Discard, execTemplateSample); err != nil {
			return fmt.Errorf("%w: Handler %s exec.%s[%d] references unsupported template field: %w", ErrActionHandlersInvalid, handlerName, field, index, err)
		}
	}
	return nil
}

// ValidateRuntime verifies that the Pool selects one built-in runtime profile.
func (s *SandboxPoolSpec) ValidateRuntime() error {
	if !IsRuntimeName(s.Runtime) {
		return fmt.Errorf("unsupported runtime %q", s.Runtime)
	}
	return nil
}

// ValidateInfraComponents verifies the cross-field constraints that the CRD
// structural schema cannot express, including component-scoped target paths
// and endpoint uniqueness.
func (s *SandboxPoolSpec) ValidateInfraComponents() error { //nolint:gocognit // pre-existing cross-field validation checks; refactor tracked separately
	if s == nil {
		return fmt.Errorf("%w: pool spec is required", ErrInfraComponentsInvalid)
	}
	names := make(map[string]struct{}, len(s.InfraComponents))
	ports := make(map[int32]string, len(s.InfraComponents))
	for index := range s.InfraComponents {
		component := &s.InfraComponents[index]
		if problems := k8svalidation.IsDNS1123Label(component.Name); len(problems) != 0 ||
			strings.HasPrefix(component.Name, "fast-sandbox-") {
			return fmt.Errorf("%w: component %d has invalid or reserved name %q", ErrInfraComponentsInvalid, index, component.Name)
		}
		if _, exists := names[component.Name]; exists {
			return fmt.Errorf("%w: duplicate component name %q", ErrInfraComponentsInvalid, component.Name)
		}
		names[component.Name] = struct{}{}

		if component.Delivery != "" && component.Delivery != InfraDeliveryHostProcess {
			return fmt.Errorf("%w: component %s delivery mode %q is unsupported", ErrInfraComponentsInvalid, component.Name, component.Delivery)
		}
		if component.Delivery == InfraDeliveryHostProcess {
			if component.Artifact != nil {
				return fmt.Errorf("%w: host-process component %s cannot declare an artifact", ErrInfraComponentsInvalid, component.Name)
			}
		} else {
			if component.Artifact == nil {
				return fmt.Errorf("%w: component %s requires an artifact unless delivery is host-process", ErrInfraComponentsInvalid, component.Name)
			}
			if err := validateComponentArtifact(component.Name, *component.Artifact); err != nil {
				return err
			}
			targetRoot := "/.fast/components/" + component.Name
			targets := make([]string, 0, len(component.Artifact.Mappings))
			for mappingIndex := range component.Artifact.Mappings {
				mapping := &component.Artifact.Mappings[mappingIndex]
				if !cleanAbsolutePath(mapping.SourcePath) {
					return fmt.Errorf("%w: component %s mapping sourcePath %q is not a clean absolute path", ErrInfraComponentsInvalid, component.Name, mapping.SourcePath)
				}
				if !cleanAbsolutePath(mapping.TargetPath) ||
					(mapping.TargetPath != targetRoot && !strings.HasPrefix(mapping.TargetPath, targetRoot+"/")) {
					return fmt.Errorf("%w: component %s mapping targetPath %q must be under %s", ErrInfraComponentsInvalid, component.Name, mapping.TargetPath, targetRoot)
				}
				for _, existing := range targets {
					if pathsOverlap(existing, mapping.TargetPath) {
						return fmt.Errorf("%w: component %s has overlapping target paths %q and %q", ErrInfraComponentsInvalid, component.Name, existing, mapping.TargetPath)
					}
				}
				targets = append(targets, mapping.TargetPath)
			}
		}

		if len(component.Process.Command) == 0 || strings.TrimSpace(component.Process.Command[0]) == "" {
			return fmt.Errorf("%w: component %s process command is required", ErrInfraComponentsInvalid, component.Name)
		}
		for name := range component.Process.Env {
			if strings.HasPrefix(name, "FAST_SANDBOX_") {
				return fmt.Errorf("%w: component %s environment key %q is reserved", ErrInfraComponentsInvalid, component.Name, name)
			}
		}
		switch component.Process.RestartPolicy {
		case "", InfraRestartNever, InfraRestartOnFailure, InfraRestartAlways:
		default:
			return fmt.Errorf("%w: component %s restart policy %q is invalid", ErrInfraComponentsInvalid, component.Name, component.Process.RestartPolicy)
		}
		health := component.Process.HealthCheck
		if (health.HTTPGet == nil) == (health.TCPConnect == nil) {
			return fmt.Errorf("%w: component %s requires exactly one health check", ErrInfraComponentsInvalid, component.Name)
		}
		if health.HTTPGet != nil && !cleanAbsolutePath(health.HTTPGet.Path) {
			return fmt.Errorf("%w: component %s health path %q is invalid", ErrInfraComponentsInvalid, component.Name, health.HTTPGet.Path)
		}
		if health.TimeoutSeconds < 0 || health.TimeoutSeconds > 300 {
			return fmt.Errorf("%w: component %s health timeout must be between 1 and 300 seconds", ErrInfraComponentsInvalid, component.Name)
		}
		if component.Endpoint.Protocol != "HTTP" {
			return fmt.Errorf("%w: component %s endpoint protocol %q is unsupported", ErrInfraComponentsInvalid, component.Name, component.Endpoint.Protocol)
		}
		if component.Endpoint.Port < 1 || component.Endpoint.Port > 65535 {
			return fmt.Errorf("%w: component %s endpoint port is invalid", ErrInfraComponentsInvalid, component.Name)
		}
		if existing, found := ports[component.Endpoint.Port]; found {
			return fmt.Errorf("%w: components %s and %s use duplicate endpoint port %d", ErrInfraComponentsInvalid, existing, component.Name, component.Endpoint.Port)
		}
		ports[component.Endpoint.Port] = component.Name
	}
	return nil
}

func cleanAbsolutePath(value string) bool {
	return strings.HasPrefix(value, "/") && value != "/" && path.Clean(value) == value
}

// validateComponentArtifact verifies the immutable artifact source and
// mapping requirements shared by every non-host-process component.
func validateComponentArtifact(componentName string, artifact InfraArtifact) error {
	source := artifact.Source
	if (source.Image == nil) == (source.Archive == nil) {
		return fmt.Errorf("%w: component %s must declare exactly one artifact source", ErrInfraComponentsInvalid, componentName)
	}
	if source.Image != nil {
		parts := strings.Split(source.Image.Reference, "@sha256:")
		if len(parts) != 2 || parts[0] == "" || !sha256Pattern.MatchString(parts[1]) {
			return fmt.Errorf("%w: component %s image reference must be digest-pinned", ErrInfraComponentsInvalid, componentName)
		}
	}
	if source.Archive != nil {
		parsed, err := url.Parse(source.Archive.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return fmt.Errorf("%w: component %s archive URL must be absolute HTTPS", ErrInfraComponentsInvalid, componentName)
		}
		if !sha256Pattern.MatchString(source.Archive.SHA256) {
			return fmt.Errorf("%w: component %s archive SHA-256 is invalid", ErrInfraComponentsInvalid, componentName)
		}
	}
	if len(artifact.Mappings) == 0 {
		return fmt.Errorf("%w: component %s requires at least one artifact mapping", ErrInfraComponentsInvalid, componentName)
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

// ValidateSandboxPoolUpdate enforces the immutable scheduling and resource
// boundary shared by admission and reconciliation.
func ValidateSandboxPoolUpdate(oldSpec, newSpec *SandboxPoolSpec) error {
	if err := oldSpec.ValidateResourceContract(); err != nil {
		return fmt.Errorf("old pool resources: %w", err)
	}
	if err := newSpec.ValidateResourceContract(); err != nil {
		return fmt.Errorf("new pool resources: %w", err)
	}
	if err := oldSpec.ValidateRuntime(); err != nil {
		return fmt.Errorf("old pool runtime: %w", err)
	}
	if err := newSpec.ValidateRuntime(); err != nil {
		return fmt.Errorf("new pool runtime: %w", err)
	}
	if err := oldSpec.ValidateInfraComponents(); err != nil {
		return fmt.Errorf("old pool Infra Components: %w", err)
	}
	if err := newSpec.ValidateInfraComponents(); err != nil {
		return fmt.Errorf("new pool Infra Components: %w", err)
	}
	if err := oldSpec.ValidateActionHandlers(); err != nil {
		return fmt.Errorf("old pool Action Handlers: %w", err)
	}
	if err := newSpec.ValidateActionHandlers(); err != nil {
		return fmt.Errorf("new pool Action Handlers: %w", err)
	}
	if oldSpec.Runtime != newSpec.Runtime {
		return ErrRuntimeImmutable
	}
	if !apiequality.Semantic.DeepEqual(oldSpec.SandboxResources, newSpec.SandboxResources) {
		return ErrResourcesImmutable
	}
	newNames := make(map[string]struct{}, len(newSpec.ActionHandlers))
	for _, handler := range newSpec.ActionHandlers {
		newNames[handler.Name] = struct{}{}
	}
	for _, handler := range oldSpec.ActionHandlers {
		if _, found := newNames[handler.Name]; !found {
			return fmt.Errorf("%w: %s", ErrActionHandlersRemoved, handler.Name)
		}
	}
	return nil
}

// NextInstanceGeneration advances a generation fence. A newly created Sandbox
// has no status yet, so its first runtime instance starts at generation one.
func NextInstanceGeneration(current int64) int64 {
	if current < InitialInstanceGeneration {
		return InitialInstanceGeneration
	}
	return current + 1
}

// Validate verifies the active placement projection required for fencing.
func (p *PlacementStatus) Validate() error {
	if p == nil {
		return errors.New("placement is required")
	}
	if p.FastletName == "" {
		return errors.New("fastletName is required")
	}
	if p.FastletPodUID == "" {
		return errors.New("fastletPodUID is required")
	}
	if p.Attempt < 1 {
		return errors.New("attempt must be at least 1")
	}
	return nil
}
