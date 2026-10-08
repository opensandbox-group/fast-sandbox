// Package main: the template-vm build command.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
)

// buildOptions holds the build-command flags. The build turns one remote
// overlaybd image into a local template pair (<output>_rootfs /
// <output>_snapfiles) under the artifact layout; nothing is pushed to a
// registry in this iteration.
const defaultBuildDiskSizeGiB = 20

type buildOptions struct {
	source          string
	output          string
	kernel          string
	kernelVersion   string
	vcpu            int
	memoryMB        int
	diskSizeGiB     int
	snapfilesSizeMB int
	rootfsDriveID   string
	bootArgs        string
	initPath        string
	initScriptFile  string
	readyPattern    string
	readyTimeout    time.Duration
	authFile        string
	username        string
	noCompress      bool
	keepWorkDir     bool
}

func newBuildCommand() *cobra.Command {
	options := &buildOptions{}
	command := &cobra.Command{
		Use:   "build --source <overlaybd-image> --output <name:tag>",
		Short: "Build a local rootfs/snapfiles template pair from a remote overlaybd image",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBuild(cmd.Context(), options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.source, "source", "", "source overlaybd image reference (required)")
	flags.StringVar(&options.output, "output", "", "logical output name <name>:<tag>; produces <tag>_rootfs and <tag>_snapfiles (required)")
	flags.StringVar(&options.kernel, "kernel", "", "vmlinux path used for the cold boot (required)")
	flags.StringVar(&options.kernelVersion, "kernel-version", "", "kernel_version recorded in metadata.json (default: derived from the vmlinux-<version> filename)")
	flags.IntVar(&options.vcpu, "vcpu", 1, "guest vCPU count")
	flags.IntVar(&options.memoryMB, "memory-mb", 1024, "guest memory in MiB")
	flags.IntVar(&options.diskSizeGiB, "disk-size-gb", defaultBuildDiskSizeGiB, "rootfs virtual disk size in GiB")
	flags.IntVar(&options.snapfilesSizeMB, "snapfiles-size-mb", 0, "snapfiles virtual disk size in MiB (default: derived from memfile + vmstate + headroom)")
	flags.StringVar(&options.rootfsDriveID, "rootfs-drive-id", defaultRootfsDriveID, "drive_id the rootfs is attached with (baked into the snapshot)")
	flags.StringVar(&options.bootArgs, "boot-args", "", "full kernel boot-args override (default: console=ttyS0 ... init=<init-path>)")
	flags.StringVar(&options.initPath, "init-path", defaultInitPath, "guest path of the injected init script")
	flags.StringVar(&options.initScriptFile, "init-script-file", "", "custom init script file (default: built-in redis script)")
	flags.StringVar(&options.readyPattern, "ready-pattern", defaultReadyPattern, "serial-log pattern marking guest workload readiness")
	flags.DurationVar(&options.readyTimeout, "ready-timeout", 3*time.Minute, "how long to wait for the ready pattern")
	flags.StringVar(&options.authFile, "auth-file", "", "docker config json used for registry authentication")
	flags.StringVar(&options.username, "username", "", "registry credentials as username:password")
	flags.BoolVar(&options.noCompress, "no-compress", false, "commit layers without zfile compression")
	flags.BoolVar(&options.keepWorkDir, "keep-work-dir", false, "keep the temporary build directory on success")
	_ = command.MarkFlagRequired("source")
	_ = command.MarkFlagRequired("output")
	_ = command.MarkFlagRequired("kernel")
	return command
}

// outputNames splits --output (<name>:<tag>) and derives the logical image
// names of the pair, mirroring template.go's rootfsRef/snapfilesRef
// convention.
type outputNames struct {
	repo       string
	templateID string
}

func parseOutput(output string) (outputNames, error) {
	repo, tag, found := strings.Cut(output, ":")
	if !found || strings.TrimSpace(repo) == "" || strings.TrimSpace(tag) == "" {
		return outputNames{}, fmt.Errorf("--output must be <name>:<tag>, got %q", output)
	}
	if strings.Contains(repo, "/") {
		return outputNames{}, fmt.Errorf("--output %q is a local logical name, not a registry reference (no '/' allowed)", output)
	}
	return outputNames{repo: repo, templateID: tag}, nil
}

func (o outputNames) rootfsName() string {
	return fmt.Sprintf("%s:%s_rootfs", o.repo, o.templateID)
}

func (o outputNames) snapfilesName() string {
	return fmt.Sprintf("%s:%s_snapfiles", o.repo, o.templateID)
}

// validateBuildOptions checks flag combinations before any side effect.
func validateBuildOptions(options *buildOptions) error {
	if strings.TrimSpace(options.source) == "" {
		return fmt.Errorf("--source is required")
	}
	if _, err := parseRegistryReference(options.source); err != nil {
		return err
	}
	if _, err := parseOutput(options.output); err != nil {
		return err
	}
	if strings.TrimSpace(options.kernel) == "" {
		return fmt.Errorf("--kernel is required")
	}
	if _, err := os.Stat(options.kernel); err != nil {
		return fmt.Errorf("kernel %s: %w", options.kernel, err)
	}
	if options.vcpu <= 0 {
		return fmt.Errorf("--vcpu must be positive")
	}
	if options.memoryMB <= 0 {
		return fmt.Errorf("--memory-mb must be positive")
	}
	if _, err := diskSizeBytes(options.diskSizeGiB); err != nil {
		return fmt.Errorf("--disk-size-gb: %w", err)
	}
	if _, err := diskSizeMiB(options.diskSizeGiB); err != nil {
		return fmt.Errorf("--disk-size-gb: %w", err)
	}
	if options.snapfilesSizeMB < 0 {
		return fmt.Errorf("--snapfiles-size-mb must not be negative")
	}
	if options.authFile != "" && options.username != "" {
		return fmt.Errorf("--auth-file and --username are mutually exclusive")
	}
	if options.username != "" {
		if _, err := parseUsernamePassword(options.username); err != nil {
			return err
		}
	}
	if _, err := kernelVersionFromPath(options.kernel, options.kernelVersion); err != nil {
		return err
	}
	if options.initScriptFile != "" {
		if _, err := os.Stat(options.initScriptFile); err != nil {
			return fmt.Errorf("init script %s: %w", options.initScriptFile, err)
		}
	}
	return nil
}

const bytesPerGiB int64 = 1 << 30

func diskSizeBytes(sizeGiB int) (int64, error) {
	if sizeGiB <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	if uint64(sizeGiB) > (^uint64(0)>>1)/uint64(bytesPerGiB) {
		return 0, fmt.Errorf("%d GiB overflows byte capacity", sizeGiB)
	}
	return int64(sizeGiB) * bytesPerGiB, nil
}

func diskSizeMiB(sizeGiB int) (int, error) {
	if sizeGiB <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	maxInt := int(^uint(0) >> 1)
	if sizeGiB > maxInt/1024 {
		return 0, fmt.Errorf("%d GiB overflows disk_size_mb", sizeGiB)
	}
	return sizeGiB * 1024, nil
}

func bytesToGiBCeil(size int64) (int, error) {
	if size <= 0 {
		return 0, fmt.Errorf("byte capacity must be positive, got %d", size)
	}
	value := (size-1)/bytesPerGiB + 1
	maxInt := int64(^uint(0) >> 1)
	if value > maxInt {
		return 0, fmt.Errorf("byte capacity %d overflows GiB size", size)
	}
	return int(value), nil
}

// snapfilesSizeGiB derives the snapfiles virtual disk size: memfile +
// vmstate + 256 MiB of ext4/metadata headroom, rounded up to whole GiB with
// a 2 GiB floor (a 1 GiB memory file alone needs more than 1 GiB of disk).
func snapfilesSizeGiB(options *buildOptions, vmstatePath, memfilePath string) (int, error) {
	if options.snapfilesSizeMB > 0 {
		return gibCeil(options.snapfilesSizeMB), nil
	}
	total, err := fileSize(memfilePath)
	if err != nil {
		return 0, err
	}
	if vmstate, err := fileSize(vmstatePath); err == nil {
		total += vmstate
	}
	const headroom = 256 * 1024 * 1024
	sizeGiB := int((total + headroom + (1 << 30) - 1) / (1 << 30))
	if sizeGiB < 2 {
		sizeGiB = 2
	}
	return sizeGiB, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return info.Size(), nil
}

// copyFile streams src to dst (the memfile can be GiB-scale, so no
// ReadFile), fsyncs dst before closing.
func copyFile(src, dst string, mode os.FileMode) error {
	source, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer source.Close()
	target, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

type buildRootfsOps struct {
	probeSourceSize  func(context.Context, string, fetchedImage) (int64, error)
	writeConfig      func(context.Context, string, fetchedImage, int) (string, error)
	addDevice        func(context.Context, string) (int, string, error)
	deleteDevice     func(context.Context, int) error
	repairFilesystem func(context.Context, string) error
	resizeFilesystem func(context.Context, string, int) error
	verifyFilesystem func(context.Context, string) error
	filesystemSize   func(context.Context, string) (int64, error)
	resizeUpper      func(context.Context, string, int) error
	deviceSize       func(string) (int64, error)
}

func newBuildRootfsOps(ublkd *ublkdClient) buildRootfsOps {
	return buildRootfsOps{
		probeSourceSize: func(ctx context.Context, workDir string, fetched fetchedImage) (int64, error) {
			return probeSourceVirtualSize(ctx, ublkd, workDir, fetched)
		},
		writeConfig:      writeBuildRootfsConfig,
		addDevice:        ublkd.Add,
		deleteDevice:     ublkd.Del,
		repairFilesystem: repairExt4,
		resizeFilesystem: resizeExt4,
		verifyFilesystem: verifyExt4,
		filesystemSize:   ext4SizeBytes,
		resizeUpper:      resizeOverlayBDUpper,
		deviceSize:       blockDeviceSize,
	}
}

type buildRootfsAttachment struct {
	devID    int
	devPath  string
	attached bool
}

func (attachment *buildRootfsAttachment) detach(ctx context.Context, ops buildRootfsOps) error {
	if !attachment.attached {
		return nil
	}
	if err := ops.deleteDevice(ctx, attachment.devID); err != nil {
		return err
	}
	attachment.attached = false
	return nil
}

func prepareBuildRootfs(ctx context.Context, workDir string, fetched fetchedImage, targetGiB int, undo *rollback, ops buildRootfsOps) (*buildRootfsAttachment, error) {
	targetBytes, err := diskSizeBytes(targetGiB)
	if err != nil {
		return nil, err
	}
	sourceBytes, err := ops.probeSourceSize(ctx, workDir, fetched)
	if err != nil {
		return nil, err
	}
	sourceGiB, err := bytesToGiBCeil(sourceBytes)
	if err != nil {
		return nil, err
	}
	initialGiB := sourceGiB
	if targetGiB > initialGiB {
		initialGiB = targetGiB
	}
	configPath, err := ops.writeConfig(ctx, workDir, fetched, initialGiB)
	if err != nil {
		return nil, err
	}
	devID, devPath, err := ops.addDevice(ctx, configPath)
	if err != nil {
		return nil, err
	}
	attachment := &buildRootfsAttachment{devID: devID, devPath: devPath, attached: true}
	undo.push(func() { _ = attachment.detach(context.Background(), ops) })

	if err := ops.repairFilesystem(ctx, attachment.devPath); err != nil {
		return nil, err
	}
	filesystemBytes, err := ops.filesystemSize(ctx, attachment.devPath)
	if err != nil {
		return nil, err
	}
	if filesystemBytes != targetBytes {
		if err := ops.resizeFilesystem(ctx, attachment.devPath, targetGiB); err != nil {
			return nil, err
		}
		if err := ops.verifyFilesystem(ctx, attachment.devPath); err != nil {
			return nil, err
		}
	}
	if initialGiB != targetGiB {
		if err := attachment.detach(ctx, ops); err != nil {
			return nil, err
		}
		if err := ops.resizeUpper(ctx, configPath, targetGiB); err != nil {
			return nil, err
		}
		devID, devPath, err = ops.addDevice(ctx, configPath)
		if err != nil {
			return nil, err
		}
		attachment.devID = devID
		attachment.devPath = devPath
		attachment.attached = true
	}
	deviceBytes, err := ops.deviceSize(attachment.devPath)
	if err != nil {
		return nil, err
	}
	if deviceBytes != targetBytes {
		return nil, fmt.Errorf("rootfs block device %s is %d bytes, want %d", attachment.devPath, deviceBytes, targetBytes)
	}
	filesystemBytes, err = ops.filesystemSize(ctx, attachment.devPath)
	if err != nil {
		return nil, err
	}
	if filesystemBytes != targetBytes {
		return nil, fmt.Errorf("rootfs ext4 filesystem on %s is %d bytes, want %d", attachment.devPath, filesystemBytes, targetBytes)
	}
	if err := ops.verifyFilesystem(ctx, attachment.devPath); err != nil {
		return nil, err
	}
	return attachment, nil
}

func prepareBuildSource(ctx context.Context, repository remoteSource, resolved resolvedBuildSource, dockerCfg dockerConfig, sourceRef string, layout artifactLayout, workDir string, diskSizeGiB int, plainHTTP bool, runner convertorRunner) (fetchedImage, error) {
	kind, err := classifyBuildSource(resolved.Manifest, sourceRef)
	if err != nil {
		return fetchedImage{}, err
	}
	switch kind {
	case buildSourceNative:
		if err := validateOverlayBDSource(resolved.Manifest, sourceRef); err != nil {
			return fetchedImage{}, err
		}
		return fetchSourceImage(ctx, repository, resolved, layout)
	case buildSourceOCI:
		credential, err := convertorCredential(dockerCfg, resolved)
		if err != nil {
			return fetchedImage{}, err
		}
		convertorDir := filepath.Join(workDir, convertedSourceDirName)
		if err := runOCIConvertor(ctx, resolved, convertorDir, diskSizeGiB, credential, plainHTTP, runner); err != nil {
			return fetchedImage{}, err
		}
		return importConvertedImage(resolved, convertorDir, layout)
	default:
		return fetchedImage{}, fmt.Errorf("unsupported source image classification %d", kind)
	}
}

// runBuild executes the build pipeline in strict order; any failure rolls
// the partially built state back in reverse (devices, mounts, workdir).
func runBuild(ctx context.Context, options *buildOptions) error {
	if err := validateBuildOptions(options); err != nil {
		return err
	}
	output, _ := parseOutput(options.output)

	ublkd := newUblkdClient(global.ublkdSocket)
	if err := ublkd.Ping(ctx); err != nil {
		return err
	}

	ref, _ := parseRegistryReference(options.source)
	dockerCfg := dockerConfig{Auths: map[string]authEntry{}}
	if options.authFile != "" {
		payload, err := os.ReadFile(options.authFile)
		if err != nil {
			return fmt.Errorf("read auth file %s: %w", options.authFile, err)
		}
		if dockerCfg, err = parseDockerConfig(string(payload)); err != nil {
			return err
		}
	} else if options.username != "" {
		entry, err := parseUsernamePassword(options.username)
		if err != nil {
			return err
		}
		dockerCfg.Auths[ref.Registry+"/"+ref.Repository] = entry
	}

	artifactName := sanitizeFileName(options.output)
	layout, finalLayout, err := createStagedArtifactLayout(global.buildOutputDir, artifactName)
	if err != nil {
		return err
	}
	undo := &rollback{}
	defer undo.run()
	stagingRoot := layout.root
	undo.push(func() { _ = os.RemoveAll(stagingRoot) })

	workDir := filepath.Join(global.buildWorkDir, artifactName)
	if err := os.RemoveAll(workDir); err != nil {
		return fmt.Errorf("clean work dir %s: %w", workDir, err)
	}
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return fmt.Errorf("create work dir %s: %w", workDir, err)
	}
	undo.push(func() {
		if !options.keepWorkDir {
			_ = os.RemoveAll(workDir)
		}
	})

	// Step 1: resolve and classify the selected platform before downloading
	// layers. Standard OCI layers are converted locally to native overlaybd.
	repository, err := newRemoteRepository(ref, dockerCfg, global.plainHTTP)
	if err != nil {
		return err
	}
	resolved, err := resolveBuildSource(ctx, repository, ref)
	if err != nil {
		return err
	}
	fetched, err := prepareBuildSource(
		ctx,
		repository,
		resolved,
		dockerCfg,
		options.source,
		layout,
		workDir,
		options.diskSizeGiB,
		global.plainHTTP,
		execConvertorRunner{},
	)
	if err != nil {
		return err
	}

	// Step 2: prepare the rootfs at the requested whole-GiB size. The source
	// filesystem is resized while the device still has max(source, target)
	// capacity; only then is a larger upper detached, resized, and reattached.
	rootfsOps := newBuildRootfsOps(ublkd)
	rootfs, err := prepareBuildRootfs(ctx, workDir, fetched, options.diskSizeGiB, undo, rootfsOps)
	if err != nil {
		return err
	}

	// Step 4: inject the init script into the rootfs.
	initScript := []byte(defaultInitScript())
	if options.initScriptFile != "" {
		if initScript, err = os.ReadFile(options.initScriptFile); err != nil {
			return fmt.Errorf("read init script %s: %w", options.initScriptFile, err)
		}
	}
	if err := injectInitScript(rootfs.devPath, filepath.Join(workDir, "mnt-rootfs"), options.initPath, initScript); err != nil {
		return err
	}

	// Step 5: cold boot, wait for readiness, capture the Full snapshot.
	vmstatePath, memfilePath, err := coldBootAndSnapshot(ctx, options, workDir, rootfs.devPath)
	if err != nil {
		return err
	}

	// Step 6: build the snapfiles device from a blank ublk disk.
	sizeGiB, err := snapfilesSizeGiB(options, vmstatePath, memfilePath)
	if err != nil {
		return err
	}
	snapConfig, err := writeBuildSnapfilesConfig(ctx, workDir, sizeGiB)
	if err != nil {
		return err
	}
	snapDevID, snapDevPath, err := ublkd.Add(ctx, snapConfig)
	if err != nil {
		return err
	}
	undo.push(func() { _ = ublkd.Del(context.Background(), snapDevID) })
	if err := mkfsExt4(ctx, snapDevPath); err != nil {
		return err
	}
	snapMount := filepath.Join(workDir, "mnt-snapfiles")
	if err := mountReadWrite(snapDevPath, snapMount); err != nil {
		return err
	}
	undo.push(func() { _ = unmount(snapMount) })
	if err := copyFile(vmstatePath, filepath.Join(snapMount, inDiskVMState), 0o640); err != nil {
		return err
	}
	if err := copyFile(memfilePath, filepath.Join(snapMount, inDiskMemfile), 0o640); err != nil {
		return err
	}
	metadataPayload, err := buildSnapfilesMetadata(options, options.source, time.Now())
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(snapMount, inDiskMetadata), metadataPayload, 0o640); err != nil {
		return fmt.Errorf("write metadata.json: %w", err)
	}
	syncFilesystems()
	if err := unmount(snapMount); err != nil {
		return err
	}
	if err := ublkd.Del(ctx, snapDevID); err != nil {
		return err
	}

	// Step 7: seal both uppers into read-only layer blobs.
	compress := !options.noCompress
	snapData, snapIndex := buildUpperPaths(workDir, "snapfiles")
	snapLayerPath := filepath.Join(workDir, "snapfiles.layer")
	if err := commitLayer(ctx, snapData, snapIndex, snapLayerPath, compress); err != nil {
		return err
	}
	snapDesc, err := installCommittedLayer(layout, snapLayerPath, compress)
	if err != nil {
		return fmt.Errorf("install snapfiles layer: %w", err)
	}

	if err := rootfs.detach(ctx, rootfsOps); err != nil {
		return err
	}
	rootfsData, rootfsIndex := buildUpperPaths(workDir, "rootfs")
	rootfsLayerPath := filepath.Join(workDir, "rootfs.layer")
	if err := commitLayer(ctx, rootfsData, rootfsIndex, rootfsLayerPath, compress); err != nil {
		return err
	}
	rootfsDesc, err := installCommittedLayer(layout, rootfsLayerPath, compress)
	if err != nil {
		return fmt.Errorf("install rootfs layer: %w", err)
	}

	// Step 8: config blob + the two manifests + layout index.
	configDesc, err := layout.installBytes(minimalImageConfig(), ocispec.MediaTypeImageConfig)
	if err != nil {
		return err
	}
	rootfsLayers := append(append([]ocispec.Descriptor{}, fetched.Layers...), rootfsDesc)
	rootfsManifest, rootfsManifestDesc, err := assembleManifest(configDesc, rootfsLayers)
	if err != nil {
		return err
	}
	snapManifest, snapManifestDesc, err := assembleManifest(configDesc, []ocispec.Descriptor{snapDesc})
	if err != nil {
		return err
	}
	rootfsFile := sanitizeFileName(output.rootfsName()) + ".json"
	snapFile := sanitizeFileName(output.snapfilesName()) + ".json"
	if err := writeFileAtomic(layout.manifestPath(rootfsFile), rootfsManifest, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", rootfsFile, err)
	}
	if err := writeFileAtomic(layout.manifestPath(snapFile), snapManifest, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", snapFile, err)
	}

	// Step 9: validate the complete staging layout before publishing its index.
	sourceManifest, err := os.ReadFile(fetched.ManifestPath)
	if err != nil {
		return fmt.Errorf("read stored source manifest: %w", err)
	}
	if err := verifyDescriptorBytes(fetched.ManifestDesc, sourceManifest, "stored source manifest"); err != nil {
		return err
	}
	if err := layout.validateLayout(sourceManifest, rootfsManifest, snapManifest); err != nil {
		return err
	}
	if err := layout.writeLayoutIndex(layoutIndex{
		SourceRef: options.source,
		SourceManifest: layoutIndexEntry{
			Name:           options.source,
			ManifestFile:   filepath.Base(fetched.ManifestPath),
			ManifestDigest: fetched.ManifestDesc.Digest.String(),
		},
		Images: []layoutIndexEntry{
			{Name: output.rootfsName(), ManifestFile: rootfsFile, ManifestDigest: rootfsManifestDesc.Digest.String()},
			{Name: output.snapfilesName(), ManifestFile: snapFile, ManifestDigest: snapManifestDesc.Digest.String()},
		},
	}); err != nil {
		return err
	}
	if err := publishArtifactLayout(layout, finalLayout); err != nil {
		return err
	}
	layout = finalLayout

	undo.disarm()
	if !options.keepWorkDir {
		_ = os.RemoveAll(workDir)
	}
	printBuildResult(layout, output, rootfsManifestDesc.Digest, snapManifestDesc.Digest)
	return nil
}

// installCommittedLayer moves an overlaybd-commit output into the blob store
// and stamps the native-overlaybd annotation on its descriptor. The
// blob-digest annotation equal to the content digest marks the blob as a
// sealed overlaybd lower for strict consumers (AgentENV convention).
func installCommittedLayer(layout artifactLayout, layerPath string, compress bool) (ocispec.Descriptor, error) {
	mediaType := overlaybdPlainLayerMediaType
	if compress {
		mediaType = overlaybdZfileLayerMediaType
	}
	desc, err := layout.installFile(layerPath, mediaType, nil)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	desc.Annotations = map[string]string{overlaybdBlobDigestAnnot: desc.Digest.String()}
	return desc, nil
}

// printBuildResult reports the produced pair on stdout.
func printBuildResult(layout artifactLayout, output outputNames, rootfsDigest, snapDigest digest.Digest) {
	fmt.Printf("built template pair from local artifacts at %s\n", layout.root)
	fmt.Printf("  %s (manifest %s)\n", output.rootfsName(), rootfsDigest)
	fmt.Printf("  %s (manifest %s)\n", output.snapfilesName(), snapDigest)
	fmt.Printf("  blobs    : %s\n", layout.blobDir())
	fmt.Printf("  manifests: %s\n", layout.manifestDir())
}
