package fastletsettings

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func TestCommitmentCapacityAnnotationRoundTrip(t *testing.T) {
	capacity := apiv1alpha2.SandboxComputeResources{
		CPU: resource.MustParse("2500m"), Memory: resource.MustParse("40Gi"),
	}
	encoded, err := EncodeCommitmentCapacity(capacity)
	require.NoError(t, err)
	require.JSONEq(t, `{"cpu":"2500m","memory":"40Gi"}`, encoded)

	decoded, err := DecodeCommitmentCapacity(encoded)
	require.NoError(t, err)
	require.Zero(t, capacity.CPU.Cmp(decoded.CPU))
	require.Zero(t, capacity.Memory.Cmp(decoded.Memory))
}

func TestCommitmentCapacityAnnotationRejectsInvalidValues(t *testing.T) {
	_, err := DecodeCommitmentCapacity(`{"cpu":"0","memory":"1Gi"}`)
	require.Error(t, err)
	_, err = DecodeCommitmentCapacity(`{"cpu":"1","memory":"bad"}`)
	require.Error(t, err)
}
