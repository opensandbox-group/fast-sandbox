package fastpath

import (
	"context"
	"fmt"
	"strconv"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/pkg/util/idgen"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	maxRetries = 3
)

// envMapToEnvVar converts map[string]string to K8s EnvVar slice
func envMapToEnvVar(envs map[string]string) []corev1.EnvVar {
	result := make([]corev1.EnvVar, 0, len(envs))
	for k, v := range envs {
		result = append(result, corev1.EnvVar{Name: k, Value: v})
	}
	return result
}

type Server struct {
	fastpathv1.UnimplementedFastPathServiceServer
	K8sClient              client.Client
	Registry               fastletpool.FastletRegistry
	FastletClient          *api.FastletClient
	DefaultConsistencyMode api.ConsistencyMode
}

// 强制编译时检查接口实现情况
var _ fastpathv1.FastPathServiceServer = &Server{}

func (s *Server) CreateSandbox(ctx context.Context, req *fastpathv1.CreateRequest) (*fastpathv1.CreateResponse, error) {
	start := time.Now()
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if req.RequestId == "" {
		generated, err := idgen.GenerateRequestID()
		if err != nil {
			return nil, err
		}
		req.RequestId = generated
	}
	if err := ValidateRequestID(req.RequestId); err != nil {
		return nil, err
	}
	createSpecHash, err := CreateSpecHash(req)
	if err != nil {
		return nil, err
	}
	if existing, err := s.findSandboxByRequestID(ctx, req.Namespace, req.RequestId); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Annotations[common.AnnotationCreateSpecHash] != createSpecHash {
			return nil, fmt.Errorf("request_id %q is already bound to a different create spec", req.RequestId)
		}
		return createResponseFromSandbox(existing), nil
	}

	mode := s.DefaultConsistencyMode
	if req.ConsistencyMode == fastpathv1.ConsistencyMode_STRONG {
		mode = api.ConsistencyModeStrong
	}

	sandboxName := req.Name
	if sandboxName == "" {
		sandboxName = fmt.Sprintf("sb-%d", time.Now().UnixNano())
	}

	klog.InfoS("FastPath CreateSandbox called", "name", sandboxName, "namespace", req.Namespace)

	tempSB := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: req.Namespace,
			Annotations: map[string]string{
				common.AnnotationRequestID:      req.RequestId,
				common.AnnotationCreateSpecHash: createSpecHash,
			},
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:        req.Image,
			PoolRef:      req.PoolRef,
			ExposedPorts: req.ExposedPorts,
			Command:      req.Command,
			Args:         req.Args,
			Envs:         envMapToEnvVar(req.Envs),
			WorkingDir:   req.WorkingDir,
		},
	}

	fastlet, err := s.Registry.Allocate(tempSB)
	if err != nil {
		klog.Error(err, "Failed to allocate fastlet for sandbox", "name", sandboxName, "namespace", req.Namespace)
		return nil, err
	}

	klog.InfoS("Fastlet allocated", "fastletID", fastlet.ID, "duration", time.Since(start))

	if mode == api.ConsistencyModeStrong {
		return s.createStrong(ctx, tempSB, fastlet, req)
	}
	return s.createFast(tempSB, fastlet, req)
}

func (s *Server) createFast(tempSB *apiv1alpha1.Sandbox, fastlet *fastletpool.FastletInfo, req *fastpathv1.CreateRequest) (*fastpathv1.CreateResponse, error) {
	start := time.Now()
	var err error
	defer func() {
		duration := time.Since(start).Seconds()
		success := "true"
		if err != nil {
			success = "false"
			klog.ErrorS(err, "Fast mode sandbox creation failed", "name", tempSB.Name, "namespace", tempSB.Namespace, "duration", duration)
		} else {
			klog.InfoS("Fast mode sandbox creation completed", "name", tempSB.Name, "namespace", tempSB.Namespace, "duration", duration)
		}
		createSandboxDuration.WithLabelValues("fast", success).Observe(duration)
	}()

	// Generate sandboxID using md5 hash
	createTimestamp := time.Now().UnixNano()
	sandboxID := idgen.GenerateHashID(tempSB.Name, tempSB.Namespace, createTimestamp)

	klog.InfoS("Creating sandbox via fastlet (fast mode)", "name", tempSB.Name, "namespace", tempSB.Namespace, "fastletPodIP", fastlet.PodIP, "fastletPod", fastlet.PodName, "sandboxID", sandboxID)

	_, err = s.FastletClient.CreateSandbox(fastlet.PodIP, &api.CreateSandboxRequest{
		Sandbox: api.SandboxSpec{
			SandboxID:  sandboxID,
			ClaimName:  tempSB.Name,
			Image:      tempSB.Spec.Image,
			Command:    tempSB.Spec.Command,
			Args:       tempSB.Spec.Args,
			Env:        req.Envs,
			WorkingDir: req.WorkingDir,
		},
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create sandbox on fastlet", "name", tempSB.Name, "namespace", tempSB.Namespace, "fastletPodIP", fastlet.PodIP)
		s.Registry.Release(fastlet.ID, tempSB)
		return nil, err
	}

	klog.InfoS("Sandbox created on fastlet, setting label and annotations", "name", tempSB.Name, "namespace", tempSB.Namespace, "fastletPod", fastlet.PodName, "node", fastlet.NodeName, "sandboxID", sandboxID)

	// 设置 label 标识 Fast 模式创建
	tempSB.SetLabels(map[string]string{
		common.LabelCreatedBy: common.CreatedByFastPathFast,
	})
	// 设置 annotations：allocation 和 createTimestamp（用于重新生成 sandboxID）
	setAnnotations(tempSB, map[string]string{
		common.AnnotationAllocation:      common.BuildAllocationJSON(fastlet.PodName, fastlet.NodeName),
		common.AnnotationCreateTimestamp: strconv.FormatInt(createTimestamp, 10),
	})

	asyncCtx, _ := context.WithTimeout(context.Background(), 30*time.Second)
	go s.asyncCreateCRDWithRetry(asyncCtx, tempSB)
	return &fastpathv1.CreateResponse{SandboxId: sandboxID, SandboxName: tempSB.Name, FastletPod: fastlet.PodName, Endpoints: s.getEndpoints(fastlet.PodIP, tempSB)}, nil
}

func (s *Server) createStrong(ctx context.Context, tempSB *apiv1alpha1.Sandbox, fastlet *fastletpool.FastletInfo, req *fastpathv1.CreateRequest) (*fastpathv1.CreateResponse, error) {
	start := time.Now()
	var err error
	defer func() {
		duration := time.Since(start).Seconds()
		success := "true"
		if err != nil {
			success = "false"
			klog.ErrorS(err, "Strong mode sandbox creation failed", "name", tempSB.Name, "namespace", tempSB.Namespace, "duration", duration)
		} else {
			klog.InfoS("Strong mode sandbox creation completed", "name", tempSB.Name, "namespace", tempSB.Namespace, "duration", duration)
		}
		createSandboxDuration.WithLabelValues("strong", success).Observe(duration)
	}()

	klog.InfoS("Creating sandbox CRD first (strong mode)", "name", tempSB.Name, "namespace", tempSB.Namespace, "fastletPod", fastlet.PodName, "node", fastlet.NodeName)

	// 设置 allocation annotation，与 CRD 创建同步
	setAnnotations(tempSB, map[string]string{
		common.AnnotationAllocation: common.BuildAllocationJSON(fastlet.PodName, fastlet.NodeName),
	})
	// Status 留空，由 Controller 从 annotation 同步

	if err = s.K8sClient.Create(ctx, tempSB); err != nil {
		klog.ErrorS(err, "Failed to create sandbox CRD", "name", tempSB.Name, "namespace", tempSB.Namespace)
		s.Registry.Release(fastlet.ID, tempSB)
		return nil, err
	}

	klog.InfoS("Sandbox CRD created, proceeding to create on fastlet", "name", tempSB.Name, "namespace", tempSB.Namespace, "uid", tempSB.UID)

	// Use UID as sandboxID
	sandboxID := string(tempSB.UID)
	tempSB.Status.SandboxID = sandboxID

	_, err = s.FastletClient.CreateSandbox(fastlet.PodIP, &api.CreateSandboxRequest{
		Sandbox: api.SandboxSpec{
			SandboxID:  sandboxID, // Changed from tempSB.Name to use UID
			ClaimUID:   string(tempSB.UID),
			ClaimName:  tempSB.Name,
			Image:      tempSB.Spec.Image,
			Command:    tempSB.Spec.Command,
			Args:       tempSB.Spec.Args,
			Env:        req.Envs,
			WorkingDir: req.WorkingDir,
		},
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create sandbox on fastlet, rolling back CRD", "name", tempSB.Name, "namespace", tempSB.Namespace, "fastletPodIP", fastlet.PodIP)
		s.K8sClient.Delete(ctx, tempSB)
		s.Registry.Release(fastlet.ID, tempSB)
		return nil, err
	}

	// After Fastlet call succeeds, update CRD status with sandboxID
	if err := s.K8sClient.Status().Update(ctx, tempSB); err != nil {
		klog.ErrorS(err, "Failed to update CRD status with sandboxID", "name", tempSB.Name, "sandboxID", sandboxID)
		// Non-fatal error, continue
	}

	klog.InfoS("Sandbox created on fastlet, Controller will sync allocation from annotation to status", "name", tempSB.Name, "namespace", tempSB.Namespace, "assignedFastlet", fastlet.PodName, "nodeName", fastlet.NodeName, "sandboxID", sandboxID)

	return &fastpathv1.CreateResponse{SandboxId: sandboxID, SandboxName: tempSB.Name, FastletPod: fastlet.PodName, Endpoints: s.getEndpoints(fastlet.PodIP, tempSB), SandboxUid: string(tempSB.UID)}, nil
}

func (s *Server) findSandboxByRequestID(ctx context.Context, namespace, requestID string) (*apiv1alpha1.Sandbox, error) {
	var list apiv1alpha1.SandboxList
	if err := s.K8sClient.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Annotations[common.AnnotationRequestID] == requestID {
			return list.Items[i].DeepCopy(), nil
		}
	}
	return nil, nil
}

func createResponseFromSandbox(sandbox *apiv1alpha1.Sandbox) *fastpathv1.CreateResponse {
	return &fastpathv1.CreateResponse{
		SandboxId:   sandbox.Status.SandboxID,
		SandboxName: sandbox.Name,
		SandboxUid:  string(sandbox.UID),
		FastletPod:  sandbox.Status.AssignedFastlet,
		Endpoints:   append([]string(nil), sandbox.Status.Endpoints...),
	}
}

func setAnnotations(sandbox *apiv1alpha1.Sandbox, values map[string]string) {
	annotations := sandbox.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, len(values))
	}
	for key, value := range values {
		annotations[key] = value
	}
	sandbox.SetAnnotations(annotations)
}

// asyncCreateCRDWithRetry 异步创建 CRD，分配信息已在 annotation 中
func (s *Server) asyncCreateCRDWithRetry(ctx context.Context, sb *apiv1alpha1.Sandbox) {
	klog.InfoS("Starting async CRD creation with retry", "name", sb.Name, "namespace", sb.Namespace, "maxRetries", maxRetries)

	for attempt := 0; attempt < maxRetries; attempt++ {
		err := s.K8sClient.Create(ctx, sb)
		if err == nil {
			klog.InfoS("Async CRD creation succeeded", "name", sb.Name, "namespace", sb.Namespace, "attempt", attempt+1)
			return
		}
		klog.InfoS("Async CRD creation failed, retrying", "name", sb.Name, "namespace", sb.Namespace, "attempt", attempt+1, "error", err)
		time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
	}
	klog.ErrorS(nil, "Async CRD creation failed after all retries", "name", sb.Name, "namespace", sb.Namespace, "maxRetries", maxRetries)
}

func (s *Server) getEndpoints(ip string, sb *apiv1alpha1.Sandbox) []string {
	var res []string
	for _, p := range sb.Spec.ExposedPorts {
		res = append(res, fmt.Sprintf("%s:%d", ip, p))
	}
	return res
}

func (s *Server) ListSandboxes(ctx context.Context, req *fastpathv1.ListRequest) (*fastpathv1.ListResponse, error) {
	namespace := req.Namespace
	klog.InfoS("Listing sandboxes", "namespace", namespace)

	var sbList apiv1alpha1.SandboxList
	if err := s.K8sClient.List(ctx, &sbList, client.InNamespace(namespace)); err != nil {
		klog.ErrorS(err, "Failed to list sandboxes", "namespace", namespace)
		return nil, err
	}

	res := &fastpathv1.ListResponse{}
	for _, sb := range sbList.Items {
		res.Items = append(res.Items, &fastpathv1.SandboxInfo{
			SandboxId:   sb.Status.SandboxID,
			SandboxName: sb.Name,
			Phase:       sb.Status.Phase,
			FastletPod:  sb.Status.AssignedFastlet,
			Endpoints:   sb.Status.Endpoints,
			Image:       sb.Spec.Image,
			PoolRef:     sb.Spec.PoolRef,
			CreatedAt:   sb.CreationTimestamp.Unix(),
		})
	}

	klog.InfoS("Listed sandboxes successfully", "namespace", namespace, "count", len(res.Items))
	return res, nil
}

func (s *Server) GetSandbox(ctx context.Context, req *fastpathv1.GetRequest) (*fastpathv1.SandboxInfo, error) {
	namespace := req.Namespace
	klog.InfoS("Getting sandbox", "name", req.SandboxName, "namespace", namespace)

	var sb apiv1alpha1.Sandbox
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: req.SandboxName, Namespace: namespace}, &sb); err != nil {
		klog.ErrorS(err, "Failed to get sandbox", "name", req.SandboxName, "namespace", namespace)
		return nil, err
	}

	return &fastpathv1.SandboxInfo{
		SandboxId:   sb.Status.SandboxID,
		SandboxName: sb.Name,
		Phase:       sb.Status.Phase,
		FastletPod:  sb.Status.AssignedFastlet,
		Endpoints:   sb.Status.Endpoints,
		Image:       sb.Spec.Image,
		PoolRef:     sb.Spec.PoolRef,
		CreatedAt:   sb.CreationTimestamp.Unix(),
	}, nil
}

func (s *Server) DeleteSandbox(ctx context.Context, req *fastpathv1.DeleteRequest) (*fastpathv1.DeleteResponse, error) {
	ns := req.Namespace
	klog.InfoS("Deleting sandbox", "name", req.SandboxName, "namespace", ns)

	sb := &apiv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: req.SandboxName, Namespace: ns}}
	if err := s.K8sClient.Delete(ctx, sb); err != nil {
		klog.ErrorS(err, "Failed to delete sandbox", "name", req.SandboxName, "namespace", ns)
		return &fastpathv1.DeleteResponse{Success: false}, err
	}

	klog.InfoS("Sandbox deleted successfully", "name", req.SandboxName, "namespace", ns)
	return &fastpathv1.DeleteResponse{Success: true}, nil
}

func (s *Server) UpdateSandbox(ctx context.Context, req *fastpathv1.UpdateRequest) (*fastpathv1.UpdateResponse, error) {
	klog.InfoS("Updating sandbox", "name", req.SandboxName, "namespace", req.Namespace)

	var sb apiv1alpha1.Sandbox
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: req.SandboxName, Namespace: req.Namespace}, &sb); err != nil {
		klog.ErrorS(err, "Failed to get sandbox for update", "name", req.SandboxName, "namespace", req.Namespace)
		return &fastpathv1.UpdateResponse{
			Success: false,
			Message: fmt.Sprintf("failed to get sandbox: %v", err),
		}, nil
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: req.SandboxName, Namespace: req.Namespace}, latest); err != nil {
			return err
		}

		switch v := req.Update.(type) {
		case *fastpathv1.UpdateRequest_ExpireTimeSeconds:
			klog.InfoS("Updating ExpireTime", "name", req.SandboxName, "expireTimeSeconds", v.ExpireTimeSeconds)
			if v.ExpireTimeSeconds == 0 {
				latest.Spec.ExpireTime = nil
			} else {
				t := metav1.NewTime(time.Unix(v.ExpireTimeSeconds, 0))
				latest.Spec.ExpireTime = &t
			}
		case *fastpathv1.UpdateRequest_ResetRevision:
			klog.InfoS("Updating ResetRevision", "name", req.SandboxName, "resetRevision", v.ResetRevision)
			t, err := time.Parse(time.RFC3339Nano, v.ResetRevision)
			if err != nil {
				return fmt.Errorf("invalid reset_revision format: %v", err)
			}
			latest.Spec.ResetRevision = &metav1.Time{Time: t}
		case *fastpathv1.UpdateRequest_FailurePolicy:
			klog.InfoS("Updating FailurePolicy", "name", req.SandboxName, "failurePolicy", v.FailurePolicy)
			latest.Spec.FailurePolicy = toFailurePolicy(v.FailurePolicy)
		case *fastpathv1.UpdateRequest_RecoveryTimeoutSeconds:
			klog.InfoS("Updating RecoveryTimeoutSeconds", "name", req.SandboxName, "recoveryTimeoutSeconds", v.RecoveryTimeoutSeconds)
			latest.Spec.RecoveryTimeoutSeconds = v.RecoveryTimeoutSeconds
		}

		// 更新标签
		if len(req.Labels) > 0 {
			klog.InfoS("Updating labels", "name", req.SandboxName, "labels", req.Labels)
			if latest.Labels == nil {
				latest.Labels = make(map[string]string)
			}
			for k, v := range req.Labels {
				latest.Labels[k] = v
			}
		}

		return s.K8sClient.Update(ctx, latest)
	})

	if err != nil {
		klog.ErrorS(err, "Failed to update sandbox", "name", req.SandboxName, "namespace", req.Namespace)
		return &fastpathv1.UpdateResponse{
			Success: false,
			Message: fmt.Sprintf("failed to update sandbox: %v", err),
		}, nil
	}

	klog.InfoS("Sandbox updated successfully", "name", req.SandboxName, "namespace", req.Namespace)

	s.K8sClient.Get(ctx, client.ObjectKey{Name: req.SandboxName, Namespace: req.Namespace}, &sb)

	return &fastpathv1.UpdateResponse{
		Success: true,
		Message: "sandbox updated successfully",
		Sandbox: &fastpathv1.SandboxInfo{
			SandboxId:   sb.Status.SandboxID,
			SandboxName: sb.Name,
			Phase:       sb.Status.Phase,
			FastletPod:  sb.Status.AssignedFastlet,
			Endpoints:   sb.Status.Endpoints,
			Image:       sb.Spec.Image,
			PoolRef:     sb.Spec.PoolRef,
			CreatedAt:   sb.CreationTimestamp.Unix(),
		},
	}, nil
}

func toFailurePolicy(fp fastpathv1.FailurePolicy) apiv1alpha1.FailurePolicy {
	switch fp {
	case fastpathv1.FailurePolicy_AUTO_RECREATE:
		return apiv1alpha1.FailurePolicyAutoRecreate
	default:
		return apiv1alpha1.FailurePolicyManual
	}
}
