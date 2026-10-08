package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// OverlayBDBSConfig is overlaybd's device config (config.v1.json). The schema
// matches accelerated-container-image pkg/types/types.go so the generated
// files are consumable by the same overlaybd runtime (ADR-003). A config with
// an empty upper describes a read-only device; a populated upper makes the
// device writable.
type OverlayBDBSConfig struct {
	RepoBlobURL string                   `json:"repoBlobUrl"`
	Lowers      []OverlayBDBSConfigLower `json:"lowers"`
	Upper       OverlayBDBSConfigUpper   `json:"upper"`
	ResultFile  string                   `json:"resultFile"`
}

// OverlayBDBSConfigLower is a lower layer entry. Remote layers are addressed
// by digest+size and fetched from RepoBlobURL at runtime.
type OverlayBDBSConfigLower struct {
	Digest string `json:"digest,omitempty"`
	Size   int64  `json:"size,omitempty"`
	File   string `json:"file,omitempty"`
	Dir    string `json:"dir,omitempty"`
}

// OverlayBDBSConfigUpper is the writable top layer (index/data on the local
// filesystem). Vsize is the virtual device size in GiB.
type OverlayBDBSConfigUpper struct {
	Index string `json:"index,omitempty"`
	Data  string `json:"data,omitempty"`
	Vsize int    `json:"vsize,omitempty"`
}

// overlaybd upper filenames inside a sandbox's upper/ directory.
const (
	upperDirName   = "upper"
	upperDataName  = "writable_data"
	upperIndexName = "writable_index"

	rootfsConfigName    = "rootfs.config.v1.json"
	snapfilesConfigName = "snapfiles.config.v1.json"
	overlaybdResultName = "result"
)

// lowersFor converts resolved remote layers into config lower entries.
func lowersFor(image resolvedImage) []OverlayBDBSConfigLower {
	lowers := make([]OverlayBDBSConfigLower, 0, len(image.Lowers))
	for _, layer := range image.Lowers {
		lowers = append(lowers, OverlayBDBSConfigLower{Digest: layer.Digest, Size: layer.Size})
	}
	return lowers
}

// writeRootfsConfig creates the hybrid writable upper (data/index) and emits
// the rootfs config.v1.json referencing it, returning the config path. The
// upper's virtual size (GiB, rounded up from the template disk size) is the
// device size overlaybd reports to the guest.
func writeRootfsConfig(ctx context.Context, home string, image resolvedImage, diskSizeMB int) (string, error) {
	upperDir := filepath.Join(home, upperDirName)
	if err := os.MkdirAll(upperDir, 0o750); err != nil {
		return "", fmt.Errorf("create upper directory: %w", err)
	}
	dataFile := filepath.Join(upperDir, upperDataName)
	indexFile := filepath.Join(upperDir, upperIndexName)
	vsizeGiB := gibCeil(diskSizeMB)
	if err := createHybridUpper(ctx, global.overlaybdCreate, dataFile, indexFile, vsizeGiB); err != nil {
		return "", err
	}
	config := OverlayBDBSConfig{
		RepoBlobURL: image.RepoBlobURL,
		Lowers:      lowersFor(image),
		Upper:       OverlayBDBSConfigUpper{Index: indexFile, Data: dataFile, Vsize: vsizeGiB},
		ResultFile:  filepath.Join(home, "rootfs."+overlaybdResultName),
	}
	return writeConfig(filepath.Join(home, rootfsConfigName), config)
}

// writeSnapfilesConfig emits the read-only snapfiles config.v1.json (no
// upper).
func writeSnapfilesConfig(home string, image resolvedImage) (string, error) {
	config := OverlayBDBSConfig{
		RepoBlobURL: image.RepoBlobURL,
		Lowers:      lowersFor(image),
		ResultFile:  filepath.Join(home, "snapfiles."+overlaybdResultName),
	}
	return writeConfig(filepath.Join(home, snapfilesConfigName), config)
}

// writeConfig serializes a config to path.
func writeConfig(path string, config OverlayBDBSConfig) (string, error) {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal overlaybd config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return "", fmt.Errorf("write overlaybd config %s: %w", path, err)
	}
	return path, nil
}

// createHybridUpper runs overlaybd-create --hybrid to allocate the LSMT
// hybrid-mode writable layer (glossary: upper). The data and index files must
// not pre-exist (overlaybd-create opens them O_EXCL); a stale pair from a
// crashed run is removed first.
func createHybridUpper(ctx context.Context, bin, dataFile, indexFile string, vsizeGiB int) error {
	_ = os.Remove(dataFile)
	_ = os.Remove(indexFile)
	arguments := []string{"--hybrid", dataFile, indexFile, strconv.Itoa(vsizeGiB)}
	command := exec.CommandContext(ctx, bin, arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("overlaybd-create --hybrid: %w: %s", err, output)
	}
	return nil
}

// gibCeil rounds a MiB size up to whole GiB, with a 1 GiB floor so a missing
// or tiny disk_size_mb still yields a usable device.
func gibCeil(sizeMB int) int {
	if sizeMB <= 0 {
		return 1
	}
	return (sizeMB + 1023) / 1024
}

// probeSourceVirtualSize attaches the downloaded source lowers read-only via
// ublkd (no upper) and reads the resulting device size: the device presents
// the source stack's virtual size, which the rootfs upper must match. The
// probe device is always detached before returning.
func probeSourceVirtualSize(ctx context.Context, ublkd *ublkdClient, workDir string, fetched fetchedImage) (size int64, retErr error) {
	config := OverlayBDBSConfig{
		Lowers:     localLowers(fetched),
		ResultFile: filepath.Join(workDir, "probe."+overlaybdResultName),
	}
	configPath := filepath.Join(workDir, "probe.config.v1.json")
	if _, err := writeConfig(configPath, config); err != nil {
		return 0, err
	}
	devID, devPath, err := ublkd.Add(ctx, configPath)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := ublkd.Del(context.Background(), devID); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("detach probe device %d: %w", devID, err))
		}
	}()
	size, err = blockDeviceSize(devPath)
	if err != nil {
		return 0, err
	}
	if size <= 0 {
		return 0, fmt.Errorf("probe device %s reports non-positive size %d", devPath, size)
	}
	return size, nil
}

// blockDeviceSize returns a block device's byte capacity. stat(2) reports an
// meaningless size for block devices, so BLKGETSIZE64 is used.
func blockDeviceSize(devPath string) (int64, error) {
	device, err := os.OpenFile(devPath, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open probe device %s: %w", devPath, err)
	}
	defer device.Close()
	var size uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0, fmt.Errorf("ioctl BLKGETSIZE64 on %s: %w", devPath, errno)
	}
	return int64(size), nil
}

// ---- build-flow device configs and commit ----

// buildUpperPaths returns the upper data/index paths of a build stage
// (rootfs or snapfiles) inside the work directory.
func buildUpperPaths(workDir, stage string) (dataFile, indexFile string) {
	return filepath.Join(workDir, stage+".upper.data"), filepath.Join(workDir, stage+".upper.index")
}

// writeBuildRootfsConfig creates the rootfs hybrid writable upper and emits
// the build-time config.v1.json whose lowers address the local blob files
// downloaded by the ORAS fetcher (no remote range reads during build).
func writeBuildRootfsConfig(ctx context.Context, workDir string, fetched fetchedImage, initialVsizeGiB int) (configPath string, err error) {
	dataFile, indexFile := buildUpperPaths(workDir, "rootfs")
	if err := createHybridUpper(ctx, global.overlaybdCreate, dataFile, indexFile, initialVsizeGiB); err != nil {
		return "", err
	}
	config := OverlayBDBSConfig{
		Lowers:     localLowers(fetched),
		Upper:      OverlayBDBSConfigUpper{Index: indexFile, Data: dataFile, Vsize: initialVsizeGiB},
		ResultFile: filepath.Join(workDir, "rootfs."+overlaybdResultName),
	}
	return writeConfig(filepath.Join(workDir, rootfsConfigName), config)
}

func resizeOverlayBDUpper(ctx context.Context, configPath string, targetGiB int) error {
	if targetGiB <= 0 {
		return fmt.Errorf("overlaybd resize target must be positive, got %d GiB", targetGiB)
	}
	arguments := []string{"--config", configPath, "--size", strconv.Itoa(targetGiB)}
	if output, err := exec.CommandContext(ctx, global.overlaybdResize, arguments...).CombinedOutput(); err != nil {
		return fmt.Errorf("overlaybd-resize %s to %d GiB: %w: %s", configPath, targetGiB, err, output)
	}
	payload, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read overlaybd config %s after resize: %w", configPath, err)
	}
	var config OverlayBDBSConfig
	if err := json.Unmarshal(payload, &config); err != nil {
		return fmt.Errorf("parse overlaybd config %s after resize: %w", configPath, err)
	}
	if config.Upper.Data == "" || config.Upper.Index == "" {
		return fmt.Errorf("overlaybd config %s has no writable upper", configPath)
	}
	config.Upper.Vsize = targetGiB
	if _, err := writeConfig(configPath, config); err != nil {
		return err
	}
	return nil
}

// writeBuildSnapfilesConfig creates an empty hybrid writable upper (no
// lowers): the device starts as a blank disk that the build formats with
// ext4 and fills with vmstate.bin/memfile/metadata.json.
func writeBuildSnapfilesConfig(ctx context.Context, workDir string, sizeGiB int) (configPath string, err error) {
	dataFile, indexFile := buildUpperPaths(workDir, "snapfiles")
	if err := createHybridUpper(ctx, global.overlaybdCreate, dataFile, indexFile, sizeGiB); err != nil {
		return "", err
	}
	config := OverlayBDBSConfig{
		Lowers:     []OverlayBDBSConfigLower{},
		Upper:      OverlayBDBSConfigUpper{Index: indexFile, Data: dataFile, Vsize: sizeGiB},
		ResultFile: filepath.Join(workDir, "snapfiles."+overlaybdResultName),
	}
	return writeConfig(filepath.Join(workDir, snapfilesConfigName), config)
}

// commitLayer seals a writable upper (data/index) into a read-only layer
// file via overlaybd-commit. compress selects zfile compression (-z, lz4 by
// default). A stale output path is removed first (overlaybd-commit opens the
// output O_EXCL; -f unlinks it).
func commitLayer(ctx context.Context, dataFile, indexFile, outputPath string, compress bool) error {
	arguments := []string{"-f"}
	if compress {
		arguments = append(arguments, "-z")
	}
	arguments = append(arguments, dataFile, indexFile, outputPath)
	command := exec.CommandContext(ctx, global.overlaybdCommit, arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("overlaybd-commit %s: %w: %s", outputPath, err, output)
	}
	return nil
}
