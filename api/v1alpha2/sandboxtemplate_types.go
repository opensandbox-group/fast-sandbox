package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxTemplatePhase is the overall build phase of a SandboxTemplate.
// +kubebuilder:validation:Enum=Pending;Building;Succeeded;Failed
type SandboxTemplatePhase string

const (
	// SandboxTemplatePhasePending means the template was accepted and the
	// build has not started yet.
	SandboxTemplatePhasePending SandboxTemplatePhase = "Pending"
	// SandboxTemplatePhaseBuilding means the build Pod is running.
	SandboxTemplatePhaseBuilding SandboxTemplatePhase = "Building"
	// SandboxTemplatePhaseSucceeded means the build finished and artifacts
	// were published.
	SandboxTemplatePhaseSucceeded SandboxTemplatePhase = "Succeeded"
	// SandboxTemplatePhaseFailed means the build failed.
	SandboxTemplatePhaseFailed SandboxTemplatePhase = "Failed"
)

// SandboxTemplateConditionType is the type of a SandboxTemplate condition.
// +kubebuilder:validation:Enum=BuildSucceeded
type SandboxTemplateConditionType string

const (
	// SandboxTemplateConditionBuildSucceeded reflects whether the last build
	// completed and its artifacts were published.
	SandboxTemplateConditionBuildSucceeded SandboxTemplateConditionType = "BuildSucceeded"
)

// SandboxTemplateCondition represents one condition of a SandboxTemplate.
type SandboxTemplateCondition struct {
	// Type is the condition type.
	// +kubebuilder:validation:Required
	Type SandboxTemplateConditionType `json:"type"`
	// Status is the condition status (True/False).
	// +kubebuilder:validation:Enum=True;False
	// +kubebuilder:validation:Required
	Status corev1.ConditionStatus `json:"status"`
	// Reason is a brief reason for the condition.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable message about the condition.
	// +optional
	Message string `json:"message,omitempty"`
	// LastTransitionTime is the last time the condition transitioned.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// MachineSpec describes the golden image's reference machine. Values are
// Kubernetes resource quantities.
type MachineSpec struct {
	// +kubebuilder:default="1"
	VCPU string `json:"vcpu"`
	// +kubebuilder:default="2Gi"
	Memory string `json:"memory"`
}

// ReadinessSpec defines when the guest is considered ready for snapshot.
// The probe takes precedence; when unset, the injected execd /ping is
// probed; as a final fallback a time-based warmup plus healthcheck applies.
type ReadinessSpec struct {
	// Probe is checked first: tcp://host:port or cmd://<command>.
	// +optional
	Probe string `json:"probe,omitempty"`
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	WarmupSeconds int32 `json:"warmupSeconds"`
	// HealthCheck is polled as the fallback; empty uses the source image
	// CMD-SHELL.
	// +optional
	HealthCheck string `json:"healthCheck,omitempty"`
}

// ArtifactFormat selects the storage encoding of the produced snapshot set.
// Both formats contain the full artifacts (rootfs.ext4, vmstate.snap,
// memory.snap); a rootfs-only output is not supported.
// +kubebuilder:validation:Enum=native;overlaybd
// +kubebuilder:default=overlaybd
type ArtifactFormat string

const (
	// ArtifactFormatNative keeps the raw snapshot files as-is.
	ArtifactFormatNative ArtifactFormat = "native"
	// ArtifactFormatOverlayBD converts rootfs and memory into OverlayBD
	// LSMT commit layers for lazy loading; vmstate.snap stays a plain file.
	ArtifactFormatOverlayBD ArtifactFormat = "overlaybd"
)

// PrimeSpec optionally selects seed nodes whose agent warms the local cache
// after a successful build. Always best-effort: it never blocks or fails the
// build, and the object store stays the authoritative source.
//
// Not yet implemented: the field is reserved — the controller currently
// ignores it. Setting it has no effect until priming lands.
type PrimeSpec struct {
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`
}

// OutputSpec describes the artifacts to produce and where to publish them.
type OutputSpec struct {
	// RootfsSize is the logical size of the converted rootfs.ext4 — the
	// capacity the guest sees as its root drive. A Kubernetes resource
	// quantity; the file is sparse. It must be large enough to hold the
	// expanded OCI image content.
	// +kubebuilder:default="30Gi"
	RootfsSize string `json:"rootfsSize"`

	// Format selects the storage encoding of the snapshot set.
	// +kubebuilder:default=overlaybd
	Format ArtifactFormat `json:"format"`

	// Publish is the S3-compatible object store target, e.g.
	// s3://bucket/sandbox-images/ (digest-addressed publication). Required:
	// without a publish target the build has no durable artifacts (they only
	// live in the build Pod's workspace).
	// +kubebuilder:validation:Required
	Publish string `json:"publish"`

	// PublishSecretRef references (imagePullSecrets-style) the secret holding
	// the object-store credentials. The secret MUST live in the platform
	// namespace (fast-sandbox-system), where the build Pod runs — a
	// SecretKeyRef cannot cross namespaces. It is created and maintained out
	// of band by the platform operator; the template only names it. Expected
	// keys: accessKeyId, secretAccessKey, endpoint, region (all required
	// when the reference is set). Secret contents are never copied into the
	// CR status or the artifact manifest.
	// +optional
	PublishSecretRef *corev1.LocalObjectReference `json:"publishSecretRef,omitempty"`

	// Registry is the OCI registry base reference for the OverlayBD image
	// artifacts produced when format=overlaybd (e.g.
	// registry.example.com/fs-templates/my-template). The builder derives
	// <Registry>-rootfs:<tag> and <Registry>-mem:<tag> from it. When set,
	// rootfs.ext4 and memory.snap are published as single-layer OverlayBD
	// OCI images instead of S3 objects; vmstate.snap and manifest.json keep
	// going to the S3 publish target. When unset, the legacy S3 layout is
	// used. Ignored for format=native.
	// +optional
	Registry string `json:"registry,omitempty"`

	// RegistrySecretRef references the secret holding the registry
	// credentials (docker config JSON, consumed as streamingvolume
	// secret.type=dockerAuth). Same namespace rules as publishSecretRef.
	// +optional
	RegistrySecretRef *corev1.LocalObjectReference `json:"registrySecretRef,omitempty"`

	// Prime optionally selects seed nodes (by label selector) whose agent
	// warms the local cache after a successful build.
	// Not yet implemented: reserved — the controller currently ignores it.
	// +optional
	Prime *PrimeSpec `json:"prime,omitempty"`
}

// SandboxTemplateSpec defines the desired golden-image build.
type SandboxTemplateSpec struct {
	// Image is the OCI image reference to convert.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// Entrypoint is the guest business command as an argv list; empty
	// defaults to ["tail","-f","/dev/null"].
	// +optional
	Entrypoint []string `json:"entrypoint,omitempty"`

	// Execd is the OpenSandbox execd image whose runtime files are injected.
	// +optional
	Execd string `json:"execd,omitempty"`

	// Kernel is the guest kernel to embed; it must be present in the builder
	// image under this name (the builder image embeds one kernel at build
	// time via the KERNEL_NAME build arg).
	// +kubebuilder:validation:Required
	Kernel string `json:"kernel"`

	// Machine describes the reference machine of the golden image.
	Machine MachineSpec `json:"machine"`

	// Init is the injected PID 1 path inside the rootfs. The init script
	// carries the readiness marker and heartbeat the build depends on, so it
	// is always injected: empty defaults to /usr/local/sbin/sandbox-init.
	// +optional
	Init string `json:"init,omitempty"`

	// Envs is injected as /etc/sandbox-init.env in the guest (literal
	// values only; valueFrom is not supported). The source image's own
	// Config.Env is not merged.
	// +optional
	Envs []corev1.EnvVar `json:"envs,omitempty"`

	// Readiness defines when the guest is considered ready.
	Readiness ReadinessSpec `json:"readiness"`

	// Output describes the artifacts to produce and where to publish them.
	Output OutputSpec `json:"output"`
}

// SandboxTemplateStatus reports the latest build outcome.
type SandboxTemplateStatus struct {
	// Phase is the overall build phase.
	Phase SandboxTemplatePhase `json:"phase,omitempty"`
	// Conditions detail progress.
	// +optional
	Conditions []SandboxTemplateCondition `json:"conditions,omitempty"`
	// ManifestRef points at the published manifest in the object store.
	// +optional
	ManifestRef string `json:"manifestRef,omitempty"`
	// ArtifactDigest is the sha256 of the manifest document itself.
	// +optional
	ArtifactDigest string `json:"artifactDigest,omitempty"`
	// RootfsImageRef is the digest-pinned OCI reference of the published
	// rootfs OverlayBD image (format=overlaybd with an output registry only).
	// +optional
	RootfsImageRef string `json:"rootfsImageRef,omitempty"`
	// MemoryImageRef is the digest-pinned OCI reference of the published
	// memory OverlayBD image (format=overlaybd with an output registry only).
	// +optional
	MemoryImageRef string `json:"memoryImageRef,omitempty"`
	// LastBuildTime is when the last build completed.
	// +optional
	LastBuildTime *metav1.Time `json:"lastBuildTime,omitempty"`
	// ObservedGeneration is the generation of the last applied build.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SandboxTemplate is a declarative golden-image build (OSEP-0023 design,
// fast-sandbox flavor): an OCI source image, runtime injection, guest init,
// readiness, and output artifacts. The build is executed by a controller
// (Phase 2) or locally by the CLI (Phase 1).
type SandboxTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxTemplateSpec   `json:"spec,omitempty"`
	Status SandboxTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxTemplateList contains a list of SandboxTemplate.
type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxTemplate{}, &SandboxTemplateList{})
}
