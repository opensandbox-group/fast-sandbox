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

func TestValidateNetworkCIDR(t *testing.T) {
	for _, valid := range []string{"", "   ", "172.30.0.0/24", "172.30.0.0/20", "10.17.0.5/16", "10.0.0.0/8", "192.168.1.0/29"} {
		require.NoError(t, ValidateNetworkCIDR(valid), "expected valid CIDR %q", valid)
	}
	for _, invalid := range []string{"not-a-cidr", "172.30.0.1", "fd00::/64", "172.30.0.0/30", "10.0.0.0/7"} {
		require.Error(t, ValidateNetworkCIDR(invalid), "expected error for invalid CIDR %q", invalid)
	}
}
