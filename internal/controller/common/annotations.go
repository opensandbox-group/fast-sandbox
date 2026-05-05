package common

import (
	"encoding/json"
	"time"
)

const (
	// LabelCreatedBy 标识 sandbox 的创建方式
	LabelCreatedBy = "sandbox.fast.io/created-by"
	// CreatedByFastPathFast 标识由 FastPath Fast 模式创建
	CreatedByFastPathFast = "fastpath-fast"

	// AnnotationAllocation 临时存储 FastPath 的分配信息，Controller 会搬运到 status 后删除
	AnnotationAllocation = "sandbox.fast.io/allocation"
	// AnnotationCreateTimestamp 存储 Fast 模式创建时的时间戳，用于重新生成 sandboxID
	AnnotationCreateTimestamp = "sandbox.fast.io/createTimestamp"
)

// AllocationInfo 临时分配信息
type AllocationInfo struct {
	AssignedFastlet string `json:"assignedFastlet"` // 分配的 Fastlet Pod
	AssignedNode    string `json:"assignedNode"`    // 分配的 Node
	AllocatedAt     string `json:"allocatedAt"`     // RFC3339 时间戳
}

// BuildAllocationJSON 构建 allocation JSON
func BuildAllocationJSON(assignedFastlet, assignedNode string) string {
	info := AllocationInfo{
		AssignedFastlet: assignedFastlet,
		AssignedNode:    assignedNode,
		AllocatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	data, _ := json.Marshal(info)
	return string(data)
}

// ParseAllocationInfo 从 annotation 解析分配信息
func ParseAllocationInfo(annotations map[string]string) (*AllocationInfo, error) {
	if annotations == nil {
		return nil, nil
	}
	data, ok := annotations[AnnotationAllocation]
	if !ok || data == "" {
		return nil, nil
	}
	var info AllocationInfo
	if err := json.Unmarshal([]byte(data), &info); err != nil {
		return nil, err
	}
	return &info, nil
}
