package fastletpool

import corev1 "k8s.io/api/core/v1"

const (
	AnnotationDraining       = "fast-sandbox.io/draining"
	AnnotationDrainStartedAt = "fast-sandbox.io/drain-started-at"
	AnnotationDrainReason    = "fast-sandbox.io/drain-reason"
	AnnotationDrainAckedAt   = "fast-sandbox.io/drain-acked-at"
)

func PodDrainRequested(pod *corev1.Pod) bool {
	return pod != nil && pod.Annotations[AnnotationDraining] == "true"
}
