package gpu

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func TestAssignedGPUPropertiesMatchEveryAllocatedUUID(t *testing.T) {
	minimum := resource.MustParse("30Gi")
	requirements := &apiv1alpha2.GPUDeviceRequirements{Model: "Tesla-V100S-PCIE-32GB", MinimumMemory: &minimum}
	good := deviceA + ", Tesla V100S-PCIE-32GB, 32768 MiB\n"
	require.NoError(t, validateDeviceProperties([]string{deviceA, deviceB}, requirements, []byte(good+deviceB+", Tesla V100S-PCIE-32GB, 32768 MiB\n")))
	require.ErrorContains(t, validateDeviceProperties([]string{deviceA, deviceB}, requirements, []byte(good+deviceB+", NVIDIA GeForce RTX 4090, 24576 MiB\n")), "does not match required model")
	require.ErrorContains(t, validateDeviceProperties([]string{deviceA, deviceB}, requirements, []byte(good+deviceB+", Tesla V100S-PCIE-32GB, 30719 MiB\n")), "below required minimum")
	require.ErrorContains(t, validateDeviceProperties([]string{deviceA, deviceB}, requirements, []byte(good)), "missing")
	require.ErrorContains(t, validateDeviceProperties([]string{deviceA, deviceB}, requirements, []byte(good+good)), "duplicate")
}

func TestKubeletDiscoveryUsesOnlyOwnedExclusiveDevices(t *testing.T) {
	pod := func(name, id string) *podresources.PodResources {
		return &podresources.PodResources{Name: name, Namespace: "ns", Containers: []*podresources.ContainerResources{{Name: "fastlet", Devices: []*podresources.ContainerDevices{{ResourceName: ResourceName, DeviceIds: []string{id}}}}}}
	}
	response := &podresources.ListPodResourcesResponse{PodResources: []*podresources.PodResources{pod("ours", deviceA), pod("other", deviceB)}}
	ids, err := devicesForContainer(response, "ns", "ours", "fastlet", 1)
	require.NoError(t, err)
	require.Equal(t, []string{deviceA}, ids)
	_, err = devicesForContainer(response, "ns", "ours", "sidecar", 1)
	require.Error(t, err)
	_, err = devicesForContainer(response, "ns", "ours", "fastlet", 2)
	require.Error(t, err)
	response.PodResources[1] = pod("other", deviceA)
	_, err = devicesForContainer(response, "ns", "ours", "fastlet", 1)
	require.ErrorContains(t, err, "unique")
	response.PodResources = []*podresources.PodResources{pod("ours", "0")}
	_, err = devicesForContainer(response, "ns", "ours", "fastlet", 1)
	require.Error(t, err)
}
