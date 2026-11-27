package reconciler

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestRemoveRuntimeOwnedEnvStripsNetworkCIDR(t *testing.T) {
	in := []corev1.EnvVar{
		{Name: "FAST_SANDBOX_NETWORK_CIDR", Value: "172.30.0.0/20"},
		{Name: "CUSTOM_USER_ENV", Value: "keep"},
	}
	got := removeRuntimeOwnedEnv(in)
	require.Equal(t, []corev1.EnvVar{{Name: "CUSTOM_USER_ENV", Value: "keep"}}, got)
}
