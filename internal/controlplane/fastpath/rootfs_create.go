package fastpath

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "fast-sandbox/api/proto/v2"
	api "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/ocitemplate"
)

// CreateRootfsSandbox pins the source before resolving capacity. The separate
// method makes an older controller reject the request before persisting intent.
func (s *Server) CreateRootfsSandbox(ctx context.Context, in *pb.CreateRootfsSandboxRequest) (*pb.CreateSandboxResponse, error) {
	if in.GetCreate() == nil {
		return nil, status.Error(codes.InvalidArgument, "create is required")
	}
	req := proto.Clone(in.Create).(*pb.CreateSandboxRequest)
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}
	ns := req.Namespace
	intent := &pb.CreateRootfsSandboxRequest{Source: in.Source}
	if supplied := req.GetResources().GetLimits(); supplied != nil {
		for _, value := range []*string{&supplied.Cpu, &supplied.Memory} {
			if *value == "" {
				continue
			}
			q, err := resource.ParseQuantity(*value)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			*value = q.String()
		}
		intent.Create = &pb.CreateSandboxRequest{Resources: req.Resources}
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(intent)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	h := sha256.Sum256(wire)
	intentHash := hex.EncodeToString(h[:])
	var prior api.Sandbox
	err = s.K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: req.RequestId}, &prior)
	if err == nil {
		pinned := prior.Annotations[assignment.AnnotationRootfsBootDigest]
		if pinned == "" || prior.Spec.Resources == nil || !strings.HasSuffix(prior.Annotations[assignment.AnnotationCreateSpecHash], ":"+intentHash) {
			return nil, status.Error(codes.AlreadyExists, "request ID belongs to a different create intent")
		}
		limits := prior.Spec.Resources.Limits
		req.Resources = &pb.SandboxResourceRequirements{Limits: &pb.ComputeResourceProfile{Cpu: limits.CPU.String(), Memory: limits.Memory.String()}}
		return s.createSandbox(ctx, req, pinned, intentHash, prior.UID)
	}
	if !apierrors.IsNotFound(err) {
		return nil, grpcKubernetesError(err)
	}
	var ref, sum, native string
	defaults := in.GetSourceLimits()
	switch {
	case in.GetSnapshotName() != "":
		var snap api.SandboxSnapshot
		if err := s.K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: in.GetSnapshotName()}, &snap); err != nil {
			return nil, grpcKubernetesError(err)
		}
		if snap.Status.Phase != api.SandboxSnapshotPhaseSucceeded || snap.Status.Mode != api.SandboxSnapshotModeRootfsOnly || !snap.DeletionTimestamp.IsZero() {
			return nil, status.Error(codes.InvalidArgument, "source must be a ready RootfsOnly snapshot")
		}
		ref, sum, native = snap.Status.ManifestRef, snap.Status.ArtifactDigest, snap.Spec.TemplateName
		if recorded := snap.Status.ResourceLimits; recorded != nil {
			defaults = &pb.ComputeResourceProfile{Cpu: recorded.CPU, Memory: recorded.Memory}
		}
	case in.GetTemplateName() != "":
		var tmpl api.SandboxTemplate
		if err := s.K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: in.GetTemplateName()}, &tmpl); err != nil {
			return nil, grpcKubernetesError(err)
		}
		if tmpl.Status.Phase != api.SandboxTemplatePhaseSucceeded || tmpl.Status.Mode != api.SandboxSnapshotModeRootfsOnly || !tmpl.DeletionTimestamp.IsZero() {
			return nil, status.Error(codes.InvalidArgument, "source must be a ready RootfsOnly template")
		}
		ref, sum, native = tmpl.Status.ManifestRef, tmpl.Status.ArtifactDigest, tmpl.Name
		defaults = &pb.ComputeResourceProfile{Cpu: tmpl.Spec.Machine.VCPU, Memory: tmpl.Spec.Machine.Memory}
	default:
		return nil, status.Error(codes.InvalidArgument, "snapshot_name or template_name required")
	}
	decoded, err := hex.DecodeString(sum)
	if err != nil || len(decoded) != 32 {
		return nil, status.Error(codes.FailedPrecondition, "source has no valid artifact digest")
	}
	if strings.HasPrefix(ref, ocitemplate.Prefix) {
		if ref != req.Image || ocitemplate.ValidateDigestReference(ref, sum) != nil {
			return nil, status.Error(codes.InvalidArgument, "source OCI identity mismatch")
		}
	} else if !strings.HasPrefix(ref, "s3://") || native != req.Image {
		return nil, status.Error(codes.InvalidArgument, "source native identity mismatch")
	}
	supplied := req.GetResources().GetLimits()
	cpu, mem := supplied.GetCpu(), supplied.GetMemory()
	if cpu == "" {
		cpu = defaults.GetCpu()
	}
	if mem == "" {
		mem = defaults.GetMemory()
	}
	limits := &pb.SandboxResourceRequirements{Limits: &pb.ComputeResourceProfile{Cpu: cpu, Memory: mem}}
	resources, err := apiSandboxResourceRequirements(limits)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	maxCPU := resource.MustParse("32")
	minCPU := resource.MustParse("10m")
	bytes := resources.Limits.Memory.Value()
	exact := bytes > 0 && resources.Limits.Memory.Cmp(*resource.NewQuantity(bytes, resource.BinarySI)) == 0
	if resources.Limits.CPU.Cmp(maxCPU) > 0 || resources.Limits.CPU.Cmp(minCPU) < 0 || !exact || bytes%(1<<20) != 0 {
		return nil, status.Error(codes.InvalidArgument, "RootfsOnly requires 10m..32 CPUs and positive whole MiB memory")
	}
	limits.Limits.Cpu = resources.Limits.CPU.String()
	limits.Limits.Memory = resources.Limits.Memory.String()
	req.Resources = limits
	return s.createSandbox(ctx, req, sum, intentHash, "")
}
