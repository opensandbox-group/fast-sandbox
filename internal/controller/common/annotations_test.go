package common

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildAllocationJSON(t *testing.T) {
	tests := []struct {
		name            string
		assignedFastlet string
		assignedNode    string
	}{
		{
			name:            "normal case",
			assignedFastlet: "fastlet-pod-1",
			assignedNode:    "node-1",
		},
		{
			name:            "different names",
			assignedFastlet: "sandbox-fastlet-abc123",
			assignedNode:    "worker-node-3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildAllocationJSON(tt.assignedFastlet, tt.assignedNode)

			var info AllocationInfo
			err := json.Unmarshal([]byte(result), &info)
			require.NoError(t, err)

			assert.Equal(t, tt.assignedFastlet, info.AssignedFastlet)
			assert.Equal(t, tt.assignedNode, info.AssignedNode)
			assert.NotEmpty(t, info.AllocatedAt)

			// 验证时间格式可以解析
			_, err = time.Parse(time.RFC3339Nano, info.AllocatedAt)
			assert.NoError(t, err, "AllocatedAt should be valid RFC3339Nano format")
		})
	}
}

func TestParseAllocationInfo(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantInfo    *AllocationInfo
		wantErr     bool
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			wantInfo:    nil,
			wantErr:     false,
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			wantInfo:    nil,
			wantErr:     false,
		},
		{
			name:        "no allocation annotation",
			annotations: map[string]string{"other": "value"},
			wantInfo:    nil,
			wantErr:     false,
		},
		{
			name:        "empty allocation annotation",
			annotations: map[string]string{AnnotationAllocation: ""},
			wantInfo:    nil,
			wantErr:     false,
		},
		{
			name:        "valid allocation annotation",
			annotations: map[string]string{AnnotationAllocation: `{"assignedFastlet":"fastlet-1","assignedNode":"node-1","allocatedAt":"2024-01-30T10:00:00Z"}`},
			wantInfo: &AllocationInfo{
				AssignedFastlet: "fastlet-1",
				AssignedNode:    "node-1",
				AllocatedAt:     "2024-01-30T10:00:00Z",
			},
			wantErr: false,
		},
		{
			name:        "invalid JSON",
			annotations: map[string]string{AnnotationAllocation: `{invalid json}`},
			wantInfo:    nil,
			wantErr:     true,
		},
		{
			name:        "partial allocation info",
			annotations: map[string]string{AnnotationAllocation: `{"assignedFastlet":"fastlet-1"}`},
			wantInfo: &AllocationInfo{
				AssignedFastlet: "fastlet-1",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ParseAllocationInfo(tt.annotations)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tt.wantInfo == nil {
				assert.Nil(t, info)
			} else {
				require.NotNil(t, info)
				assert.Equal(t, tt.wantInfo.AssignedFastlet, info.AssignedFastlet)
				assert.Equal(t, tt.wantInfo.AssignedNode, info.AssignedNode)
				assert.Equal(t, tt.wantInfo.AllocatedAt, info.AllocatedAt)
			}
		})
	}
}

func TestBuildParseRoundTrip(t *testing.T) {
	// 验证 BuildAllocationJSON 和 ParseAllocationInfo 可以往返
	assignedFastlet := "test-fastlet-pod"
	assignedNode := "test-node"

	jsonStr := BuildAllocationJSON(assignedFastlet, assignedNode)
	info, err := ParseAllocationInfo(map[string]string{AnnotationAllocation: jsonStr})

	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, assignedFastlet, info.AssignedFastlet)
	assert.Equal(t, assignedNode, info.AssignedNode)
}
