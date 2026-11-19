package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxTemplatePhase is the overall build phase of a SandboxTemplate.
// +kubebuilder:validation:Enum=Pending;Building;Succeeded;Failed
type SandboxTemplatePhase string

// SandboxTemplateArtifactModeAnnotation reports the artifact published by a build Pod.
const SandboxTemplateArtifactModeAnnotation = "sandbox.fast.io/artifact-mode"

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
// Native publishes raw snapshots to object storage; OverlayBD publishes a
// rootfs/snapfiles image pair to an OCI registry.
// +kubebuilder:validation:Enum=native;overlaybd
// +kubebuilder:default=overlaybd
type ArtifactFormat string

const (
	// ArtifactFormatNative keeps the raw snapshot files in object storage.
	ArtifactFormatNative ArtifactFormat = "native"
	// ArtifactFormatOverlayBD publishes OverlayBD rootfs and snapfiles images
	// to an OCI registry for lazy loading. Object storage is not supported.
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
// Cross-field validation runs in the controller so these CRDs ship without CEL
// (x-kubernetes-validations) for legacy (< 1.25) API servers.
type OutputSpec struct {
	// Mode selects the published boot artifact. RootfsOnly requires native storage.
	// +optional
	Mode SandboxSnapshotMode `json:"mode,omitempty"`

	// RootfsSize is the logical size of the converted rootfs.ext4 — the
	// capacity the guest sees as its root drive. A Kubernetes resource
	// quantity; the file is sparse. It must be large enough to hold the
	// expanded OCI image content.
	// +kubebuilder:default="30Gi"
	RootfsSize string `json:"rootfsSize"`

	// Format selects the storage encoding. Native requires object storage;
	// OverlayBD requires an explicit oci:// publish target.
	// +kubebuilder:default=overlaybd
	Format ArtifactFormat `json:"format"`

	// Publish selects the destination. oci://registry/namespace/repo must
	// omit the tag and digest; each build Pod receives a system-generated
	// UUID and publishes <uuid>_rootfs and <uuid>_snapfiles. A new build
	// uses a new UUID; retries of the same Pod reuse its UUID.
	// PublishSecretRef then requires .dockerconfigjson.
	// For format=native, Publish is the S3-compatible object store target, e.g.
	// s3://bucket/sandbox-images/ (digest-addressed publication). When
	// empty, the controller defaults it to the platform artifact store
	// (the fast-sandbox-artifact-store ConfigMap); when set it must equal
	// that store, so a template cannot silently drift away from the store
	// the node agents read. Without either, the build fails with an
	// invalid-output condition (durable artifacts have no destination).
	// +optional
	Publish string `json:"publish,omitempty"`

	// PublishSecretRef references (imagePullSecrets-style) the secret holding
	// the object-store credentials. The secret MUST live in the template's
	// namespace, where the build Pod runs (a Pod can only mount Secrets from
	// its own namespace). It is created and maintained out of band by the
	// platform operator; the template only names it. Expected keys:
	// accessKeyId, secretAccessKey, endpoint, region (all required when the
	// reference is set). The controller mounts the Secret into the build Pod
	// and the builder reads the keys as files; secret contents are never
	// copied into the CR status, the Pod env, or the artifact manifest.
	// +optional
	PublishSecretRef *corev1.LocalObjectReference `json:"publishSecretRef,omitempty"`

	// Prime optionally selects seed nodes (by label selector) whose agent
	// warms the local cache after a successful build.
	// Not yet implemented: reserved — the controller currently ignores it.
	// +optional
	Prime *PrimeSpec `json:"prime,omitempty"`
}

// MITMCertificateSpec selects the Secret entry holding the egress MITM
// proxy CA certificate (OSEP-0022).
type MITMCertificateSpec struct {
	// SecretRef references the Secret carrying the certificate; it must
	// live in the template's namespace.
	// +kubebuilder:validation:Required
	SecretRef corev1.LocalObjectReference `json:"secretRef"`
	// Key is the Secret data entry holding the PEM certificate. Only this
	// key is projected into the build Pod, so the CA private key never
	// enters the build. The builder accepts entries consisting solely of
	// PEM CERTIFICATE blocks — private keys, other PEM blocks, and trailing
	// non-PEM data are rejected — and bakes them re-encoded; CA constraints
	// (BasicConstraints CA:TRUE) are not verified.
	// +kubebuilder:default=ca.crt
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
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

	// IndexKey optionally names an additional content-addressed image-index
	// key the publish stage writes alongside the default sha256(image) key.
	// Use case: per-template artifact identity — the OpenSandbox server sets
	// it to the template ID so two templates of the same source image never
	// alias each other's artifact sets or node caches. The index payload's
	// image field equals the key it is written under, so pull-side byte
	// matching works unchanged. The default key stays last-writer-wins for
	// warmImages and older clients.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(/[a-zA-Z0-9._-]+)*(:[a-zA-Z0-9._-]{1,127})?$`
	// +kubebuilder:validation:MaxLength=255
	// +optional
	IndexKey string `json:"indexKey,omitempty"`

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
	// OCI Config.Env (the merged Dockerfile ENV) is inherited like a
	// container runtime would; an env with the same name here overrides
	// the inherited value.
	// +optional
	Envs []corev1.EnvVar `json:"envs,omitempty"`

	// EgressMITMCertificate bakes the egress MITM proxy CA certificate into
	// the template: the builder places it at /opt/opensandbox/mitmproxy-ca-cert.pem
	// (the execd bootstrap contract path) and enables the trust gate, so
	// the bootstrap installs it into the guest trust store and client env
	// vars at the snapshot boot. Restored sandboxes trust the CA from first
	// boot; the CA is cluster-global, so the field is pool-agnostic.
	// +optional
	EgressMITMCertificate *MITMCertificateSpec `json:"egressMitmCertificate,omitempty"`

	// Readiness defines when the guest is considered ready.
	Readiness ReadinessSpec `json:"readiness"`

	// Output describes the artifacts to produce and where to publish them.
	Output OutputSpec `json:"output"`
}

// SandboxTemplateStatus reports the latest build outcome.
type SandboxTemplateStatus struct {
	// Mode is the artifact class confirmed by the builder. Empty means unknown.
	Mode SandboxSnapshotMode `json:"mode,omitempty"`
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
