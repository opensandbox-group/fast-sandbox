package ocitemplate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
)

const (
	RootfsUpperName         = "rootfs-upper.json"
	UpperDataName           = "rootfs.upper.data"
	UpperIndexName          = "rootfs.upper.index"
	RemoteSourcesName       = "remote-sources.json"
	OverlayBDLayerMediaType = "application/vnd.containerd.overlaybd.image.layer.v1.zfile"
)

// RootfsUpper describes a frozen private upper and its immutable parent stack.
// It contains no credentials and never requires reading the lower data blocks.
type RootfsUpper struct {
	Repository       string           `json:"repository"`
	VirtualSizeBytes int64            `json:"virtualSizeBytes"`
	Lowers           []oci.Descriptor `json:"lowers"`
	// BaseLayers is the number of bottom Lowers that belong to the immutable
	// template base and must stay shared across sandboxes. Zero means the
	// boundary is unknown (legacy captures); the squash policy then falls
	// back to the positional tail rule.
	BaseLayers int `json:"baseLayers,omitempty"`
}

func (u RootfsUpper) Validate() error {
	ref, err := registry.ParseReference(u.Repository)
	if err != nil || ref.Reference != "" {
		return fmt.Errorf("invalid rootfs parent repository")
	}
	if u.VirtualSizeBytes <= 0 || len(u.Lowers) == 0 {
		return fmt.Errorf("invalid rootfs upper size or empty parent stack")
	}
	if u.BaseLayers < 0 || u.BaseLayers > len(u.Lowers) {
		return fmt.Errorf("invalid rootfs base layer count %d for %d parent layers", u.BaseLayers, len(u.Lowers))
	}
	for _, d := range u.Lowers {
		if d.Digest.Algorithm() != digest.SHA256 || d.Digest.Validate() != nil || d.Size <= 0 || d.MediaType != OverlayBDLayerMediaType {
			return fmt.Errorf("invalid OverlayBD parent descriptor")
		}
	}
	return nil
}

func ReadRootfsUpper(dir string) (*RootfsUpper, error) {
	raw, err := os.ReadFile(filepath.Join(dir, RootfsUpperName))
	if err != nil {
		return nil, err
	}
	var upper RootfsUpper
	if err = json.Unmarshal(raw, &upper); err != nil {
		return nil, err
	}
	if err = upper.Validate(); err != nil {
		return nil, err
	}
	return &upper, nil
}
