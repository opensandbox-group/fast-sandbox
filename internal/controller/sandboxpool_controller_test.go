package controller

import (
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
)

func TestGetRuntimeClassName(t *testing.T) {
	tests := []struct {
		name     string
		pool     *apiv1alpha1.SandboxPool
		expected string
	}{
		{
			name: "default container runtime returns empty",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeContainer},
			},
			expected: "",
		},
		{
			name: "empty runtime type returns empty",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: ""},
			},
			expected: "",
		},
		{
			name: "gvisor returns gvisor",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeGVisor},
			},
			expected: "gvisor",
		},
		{
			name: "kata-qemu returns kata-qemu",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataQemu},
			},
			expected: "kata-qemu",
		},
		{
			name: "kata-fc returns kata-fc",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataFc},
			},
			expected: "kata-fc",
		},
		{
			name: "kata-clh returns kata-clh",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataClh},
			},
			expected: "kata-clh",
		},
		{
			name: "custom RuntimeClassName overrides default",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{
					RuntimeType:      apiv1alpha1.RuntimeGVisor,
					RuntimeClassName: "custom-gvisor",
				},
			},
			expected: "custom-gvisor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getRuntimeClassName(tt.pool)
			if result != tt.expected {
				t.Errorf("getRuntimeClassName() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestGetContainerdRuntimeHandler(t *testing.T) {
	tests := []struct {
		name     string
		pool     *apiv1alpha1.SandboxPool
		expected string
	}{
		{
			name: "container returns runc handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeContainer},
			},
			expected: "io.containerd.runc.v2",
		},
		{
			name: "empty runtime type returns runc handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: ""},
			},
			expected: "io.containerd.runc.v2",
		},
		{
			name: "gvisor returns runsc handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeGVisor},
			},
			expected: "io.containerd.runsc.v1",
		},
		{
			name: "kata-qemu returns kata-qemu handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataQemu},
			},
			expected: "io.containerd.kata-qemu.v2",
		},
		{
			name: "kata-fc returns kata-fc handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataFc},
			},
			expected: "io.containerd.kata-fc.v2",
		},
		{
			name: "kata-clh returns kata-clh handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataClh},
			},
			expected: "io.containerd.kata-clh.v2",
		},
		{
			name: "custom handler overrides default",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{
					RuntimeType:              apiv1alpha1.RuntimeGVisor,
					ContainerdRuntimeHandler: "custom-handler",
				},
			},
			expected: "custom-handler",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getContainerdRuntimeHandler(tt.pool)
			if result != tt.expected {
				t.Errorf("getContainerdRuntimeHandler() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestGetRuntimeType(t *testing.T) {
	tests := []struct {
		name     string
		pool     *apiv1alpha1.SandboxPool
		expected apiv1alpha1.RuntimeType
	}{
		{
			name: "empty runtime type returns container",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: ""},
			},
			expected: apiv1alpha1.RuntimeContainer,
		},
		{
			name: "specified runtime type is returned",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeGVisor},
			},
			expected: apiv1alpha1.RuntimeGVisor,
		},
		{
			name: "kata-qemu runtime type is returned",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataQemu},
			},
			expected: apiv1alpha1.RuntimeKataQemu,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getRuntimeType(tt.pool)
			if result != tt.expected {
				t.Errorf("getRuntimeType() = %q, want %q", result, tt.expected)
			}
		})
	}
}
