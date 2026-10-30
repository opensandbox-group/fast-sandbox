package firecracker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/ocitemplate"
)

// captureOCIUpper runs only after snapshot creation has drained/flushed the
// drive, while vCPUs are paused. It never modifies the live writable layer.
func captureOCIUpper(ociRoot, sandboxID, staging string) error {
	home := filepath.Join(ociRoot, "sandboxes", sandboxID)
	raw, err := os.ReadFile(filepath.Join(home, "rootfs.config.v1.json"))
	if err != nil {
		return err
	}
	var config struct {
		RepoBlobURL string `json:"repoBlobUrl"`
		Lowers      []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"lowers"`
		Upper struct {
			Data  string `json:"data"`
			Index string `json:"index"`
		} `json:"upper"`
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		return err
	}
	endpoint, err := url.Parse(config.RepoBlobURL)
	if err != nil || endpoint.Host == "" || !strings.HasPrefix(endpoint.Path, "/v2/") || !strings.HasSuffix(endpoint.Path, "/blobs") {
		return fmt.Errorf("invalid OverlayBD parent blob URL")
	}
	repository := endpoint.Host + strings.TrimSuffix(strings.TrimPrefix(endpoint.Path, "/v2"), "/blobs")
	raw, err = os.ReadFile(filepath.Join(home, "mnt/manifest.json"))
	if err != nil {
		return err
	}
	manifest, err := artifacts.DecodeVMManifest(raw)
	if err != nil {
		return err
	}
	upper := ocitemplate.RootfsUpper{Repository: repository, VirtualSizeBytes: manifest.Machine.RootfsSizeMiB << 20}
	for _, lower := range config.Lowers {
		upper.Lowers = append(upper.Lowers, oci.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.Digest(lower.Digest), Size: lower.Size})
	}
	// The template base boundary was recorded at attach time from the
	// published rootfs manifest; carrying it into the upper keeps the squash
	// policy from ever merging shared template base layers.
	state, err := readSandboxBaseLayers(home)
	if err != nil {
		return err
	}
	upper.BaseLayers = state
	if err = upper.Validate(); err != nil {
		return err
	}
	for _, entry := range []struct{ source, name string }{{config.Upper.Data, ocitemplate.UpperDataName}, {config.Upper.Index, ocitemplate.UpperIndexName}} {
		// Upper files are owned by this sandbox, not arbitrary config paths.
		relative, e := filepath.Rel(filepath.Join(home, "upper"), entry.source)
		if e != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("rootfs upper outside sandbox directory")
		}
		if err = copyFrozenUpper(entry.source, filepath.Join(staging, entry.name)); err != nil {
			return err
		}
	}
	raw, err = json.Marshal(upper)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(staging, ocitemplate.RootfsUpperName), raw, 0600)
}

// readSandboxBaseLayers reads the template base boundary fsb-vm persisted in
// the sandbox state at attach time. Legacy states without the field mean
// unknown (0).
func readSandboxBaseLayers(home string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		return 0, err
	}
	var state struct {
		RootfsBaseLayers int `json:"rootfs_base_layers,omitempty"`
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return 0, fmt.Errorf("parse sandbox state %s: %w", filepath.Join(home, "state.json"), err)
	}
	return state.RootfsBaseLayers, nil
}

// Hybrid uppers have a large sparse address space. Copying only allocated
// extents avoids scanning/materializing holes and never reads the remote base.
func copyFrozenUpper(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("invalid writable upper %s", source)
	}
	if err = reflinkOnly(source, target); err == nil {
		return nil
	}
	_ = os.Remove(target)
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	if err = output.Truncate(info.Size()); err != nil {
		return err
	}
	if err = copyUpperExtents(input, output, info.Size()); err != nil {
		return err
	}

	return output.Close()
}

func copyUpperExtents(input, output *os.File, length int64) error {
	return copyUpperExtentsWithSeek(input, output, length, unix.Seek)
}

func copyUpperExtentsWithSeek(input, output *os.File, length int64, seek func(int, int64, int) (int64, error)) error {
	var err error
	for offset := int64(0); offset < length; {
		data, e := seek(int(input.Fd()), offset, unix.SEEK_DATA)
		if errors.Is(e, unix.ENXIO) {
			break
		}
		// A filesystem without extent seeking can still export a consistent upper;
		// this fallback copies the private upper, never the full stacked rootfs.
		if errors.Is(e, unix.EINVAL) || errors.Is(e, unix.ENOTSUP) {
			return copyUpperSparseScan(input, output, length)
		}
		if e != nil {
			return e
		}
		hole, e := seek(int(input.Fd()), data, unix.SEEK_HOLE)
		if errors.Is(e, unix.EINVAL) || errors.Is(e, unix.ENOTSUP) {
			return copyUpperSparseScan(input, output, length)
		}
		if e != nil {
			return e
		}
		if hole > length {
			hole = length
		}
		if hole <= data {
			return fmt.Errorf("invalid sparse upper extent")
		}
		if _, err = input.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err = output.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err = io.CopyN(output, input, hole-data); err != nil {
			return err
		}
		offset = hole
	}
	return nil
}

// copyUpperSparseScan scans in bounded blocks when extent seeking is unavailable.
// Skipping all-zero blocks preserves holes even for large logical uppers.
func copyUpperSparseScan(input, output *os.File, length int64) error {
	buffer := make([]byte, 1<<20)
	zeros := make([]byte, 4096)
	for offset := int64(0); offset < length; {
		size := int64(len(buffer))
		if length-offset < size {
			size = length - offset
		}
		chunk := buffer[:size]
		if _, err := input.ReadAt(chunk, offset); err != nil {
			return err
		}
		for start := 0; start < len(chunk); start += len(zeros) {
			end := start + len(zeros)
			if end > len(chunk) {
				end = len(chunk)
			}
			block := chunk[start:end]
			if !bytes.Equal(block, zeros[:len(block)]) {
				if _, err := output.WriteAt(block, offset+int64(start)); err != nil {
					return err
				}
			}
		}
		offset += size
	}
	return nil
}
