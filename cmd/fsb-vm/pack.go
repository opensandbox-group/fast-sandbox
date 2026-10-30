package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/registry/remote/auth"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/ocitemplate"
)

// Checkpoint rootfs chain policy: at maxRootfsLayers, merge the newest
// squashLayers layers into one; template base layers stay at the bottom.
const (
	defaultMaxRootfsLayers = 24
	defaultSquashLayers    = 16
)

// pack exports an already captured checkpoint. It never boots a new VM.
func newPackCommand() *cobra.Command {
	var bundle, output string
	var maxLayers, squashLayers int
	cmd := &cobra.Command{Use: "pack", Short: "Package captured rootfs, memory and VM state as an OverlayBD pair", RunE: func(cmd *cobra.Command, _ []string) error {
		if squashLayers < 2 {
			return fmt.Errorf("--squash-layers must be at least 2, got %d", squashLayers)
		}
		if maxLayers <= squashLayers {
			return fmt.Errorf("--max-rootfs-layers (%d) must exceed --squash-layers (%d)", maxLayers, squashLayers)
		}
		return packCheckpoint(cmd.Context(), bundle, output, maxLayers, squashLayers)
	}}
	cmd.Flags().StringVar(&bundle, "bundle-dir", "", "captured checkpoint directory")
	cmd.Flags().StringVar(&output, "output", "", "local logical output name:tag")
	cmd.Flags().IntVar(&maxLayers, "max-rootfs-layers", defaultMaxRootfsLayers, "squash the inherited rootfs layer chain once it reaches this many layers")
	cmd.Flags().IntVar(&squashLayers, "squash-layers", defaultSquashLayers, "number of newest rootfs layers merged per squash")
	_ = cmd.MarkFlagRequired("bundle-dir")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func packCheckpoint(ctx context.Context, bundle, outputName string, maxLayers, squashLayers int) (resultErr error) {
	releaseBundle, err := lockPackBundle(bundle)
	if err != nil {
		return err
	}
	defer releaseBundle()
	if err := recoverPack(ctx, bundle, newUblkdClient(global.ublkdSocket), unmount); err != nil {
		return err
	}
	output, err := parseOutput(outputName)
	if err != nil {
		return err
	}
	metadata, upper, err := validatePackBundle(bundle)
	if err != nil {
		return err
	}
	extras, err := packSnapshotExtras(bundle)
	if err != nil {
		return err
	}
	name := sanitizeFileName(outputName)
	release, err := tryLock(global.buildOutputDir, name)
	if err != nil {
		return err
	}
	defer release()
	layout, final, err := createStagedArtifactLayout(global.buildOutputDir, name)
	if err != nil {
		return err
	}
	undo := &rollback{}
	activeMarker := filepath.Join(bundle, packActiveMarker)
	defer func() {
		if e := undo.run(); e != nil {
			resultErr = errors.Join(resultErr, e)
		} else if e := os.Remove(activeMarker); e != nil && !os.IsNotExist(e) {
			resultErr = errors.Join(resultErr, e)
		}
	}()
	undo.push("staging "+layout.root, func() error { return os.RemoveAll(layout.root) })
	work, err := createBuildWorkDir(name, false, undo)
	if err != nil {
		return err
	}
	rootLayers, err := packRootfs(ctx, bundle, work, layout, upper, maxLayers, squashLayers)
	if err != nil {
		return err
	}
	ublkd := newUblkdClient(global.ublkdSocket)
	options := &buildOptions{snapshotManifest: metadata, snapshotExtras: extras, snapfilesProgress: func(phase string) error { return writePackResources(activeMarker, work, phase) }}
	// Preserve backing files if this process is killed or resource rollback fails.
	if err = writePackResources(activeMarker, work, packPhasePrepared); err != nil {
		return err
	}
	if err = captureSnapfiles(ctx, ublkd, options, work, filepath.Join(bundle, "vmstate.snap"), filepath.Join(bundle, "memory.snap"), undo); err != nil {
		return err
	}
	snap, err := sealUpper(ctx, layout, work, "snapfiles", true)
	if err != nil {
		return err
	}
	if _, err = writeTemplatePair(layout, output, rootLayers, snap, "checkpoint", nil); err != nil {
		return err
	}
	if err = publishArtifactLayout(layout, final); err != nil {
		return err
	}
	// Device and mount releases are idempotent; keep rollback armed until every
	// kernel resource has been released, then discard only the scratch directory.
	return nil
}

// State files keep the native snapshot names and opaque contents in snapfiles.
func packSnapshotExtras(bundle string) (map[string]string, error) {
	extras := map[string]string{}
	for _, name := range []string{"vault.json", "policy.json"} {
		path := filepath.Join(bundle, name)
		if _, err := os.Stat(path); err == nil {
			extras[name] = path
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat snapshot %s: %w", name, err)
		}
	}
	return extras, nil
}

func validatePackBundle(bundle string) ([]byte, *ocitemplate.RootfsUpper, error) {
	metadata, err := os.ReadFile(filepath.Join(bundle, inDiskManifest))
	if err != nil {
		return nil, nil, err
	}
	manifest, err := artifacts.DecodeVMManifest(metadata)
	if err != nil {
		return nil, nil, err
	}
	machine := manifest.Machine

	upper, upperErr := ocitemplate.ReadRootfsUpper(bundle)
	if upperErr != nil && !errors.Is(upperErr, os.ErrNotExist) {
		return nil, nil, upperErr
	}
	required := []string{"vmstate.snap", "memory.snap", "manifest.json"}
	sizes := map[string]int64{"memory.snap": int64(machine.MemoryMiB) << 20}
	if upper != nil {
		if upper.VirtualSizeBytes != int64(machine.RootfsSizeMiB)<<20 {
			return nil, nil, fmt.Errorf("rootfs upper size does not match machine metadata")
		}
		required = append(required, ocitemplate.UpperDataName, ocitemplate.UpperIndexName)
	} else {
		required = append(required, "rootfs.ext4")
		sizes["rootfs.ext4"] = int64(machine.RootfsSizeMiB) << 20
	}
	for _, name := range required {
		if info, err := os.Stat(filepath.Join(bundle, name)); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, nil, fmt.Errorf("checkpoint is missing non-empty regular %s", name)
		}
	}
	for name, want := range sizes {
		info, err := os.Stat(filepath.Join(bundle, name))
		if err != nil {
			return nil, nil, err
		}
		if info.Size() != want {
			return nil, nil, fmt.Errorf("checkpoint %s size %d does not match machine metadata %d", name, info.Size(), want)
		}
	}

	return metadata, upper, nil
}

func packRootfs(ctx context.Context, bundle, work string, layout artifactLayout, upper *ocitemplate.RootfsUpper, maxLayers, squashLayers int) ([]ocispec.Descriptor, error) {
	compressed := filepath.Join(work, "rootfs.zfile")
	var rootLayers []ocispec.Descriptor
	if upper != nil {
		if err := commitLayer(ctx, filepath.Join(bundle, ocitemplate.UpperDataName), filepath.Join(bundle, ocitemplate.UpperIndexName), compressed, true); err != nil {
			return nil, err
		}
		rootLayers = append(rootLayers, upper.Lowers...)
		sources := map[string]string{}
		for _, lower := range upper.Lowers {
			sources[lower.Digest.String()] = upper.Repository
		}
		raw, e := json.Marshal(sources)
		if e != nil {
			return nil, e
		}
		if err := os.WriteFile(filepath.Join(layout.root, ocitemplate.RemoteSourcesName), raw, 0600); err != nil {
			return nil, err
		}
	} else {
		rawLayer := filepath.Join(work, "rootfs.sealed")
		importer := exec.CommandContext(ctx, "overlaybd-import-raw", filepath.Join(bundle, "rootfs.ext4"), filepath.Join(work, "rootfs.data"), filepath.Join(work, "rootfs.index"), rawLayer)
		if output, e := importer.CombinedOutput(); e != nil {
			return nil, fmt.Errorf("import checkpoint rootfs: %w: %s", e, output)
		}
		if output, e := exec.CommandContext(ctx, global.overlaybdCommit, "-f", "-z", "--commit_sealed", rawLayer, compressed).CombinedOutput(); e != nil {
			return nil, fmt.Errorf("compress checkpoint rootfs: %w: %s", e, output)
		}
	}
	root, err := installCommittedLayer(layout, compressed, true)
	if err != nil {
		return nil, err
	}
	rootLayers = append(rootLayers, root)
	return squashRootfsTail(ctx, layout, rootLayers, upper, maxLayers, squashLayers)
}

// squashRootfsTail merges the newest squashLayers layers into one once the
// chain reaches maxLayers, keeping the template base layers at the bottom.
// Missing tail blobs are fetched from upper.Repository; any failure skips
// the squash and publishes the un-squashed chain.
func squashRootfsTail(ctx context.Context, layout artifactLayout, rootLayers []ocispec.Descriptor, upper *ocitemplate.RootfsUpper, maxLayers, squashLayers int) ([]ocispec.Descriptor, error) {
	if upper == nil || len(rootLayers) < maxLayers {
		return rootLayers, nil
	}
	tailCount := squashLayers
	if tailCount > len(rootLayers)-1 {
		tailCount = len(rootLayers) - 1
	}
	bottom := rootLayers[:len(rootLayers)-tailCount]
	tail := rootLayers[len(rootLayers)-tailCount:]
	for _, layer := range tail {
		if err := ensureLocalBlob(ctx, layout, upper.Repository, layer); err != nil {
			return rootLayers, squashMiss(err)
		}
	}
	merged := filepath.Join(layout.blobDir(), fmt.Sprintf(".squash-%s.tmp", digest.FromString(filepath.Base(layout.root)).Encoded()))
	arguments := []string{"-n", strconv.Itoa(tailCount), "-z"}
	for _, layer := range tail {
		arguments = append(arguments, layout.blobPath(layer.Digest))
	}
	arguments = append(arguments, merged)
	command := exec.CommandContext(ctx, global.overlaybdMerge, arguments...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Stdout = io.Discard
	defer os.Remove(merged)
	if err := command.Run(); err != nil {
		return rootLayers, squashMiss(fmt.Errorf("overlaybd-merge: %w: %s", err, stderr.String()))
	}
	desc, err := installCommittedLayer(layout, merged, true)
	if err != nil {
		return rootLayers, squashMiss(err)
	}
	fmt.Fprintf(os.Stderr, "squashed %d rootfs layers into %s\n", tailCount, desc.Digest)
	return append(bottom, desc), nil
}

// squashMiss turns a failed squash into an optimization miss.
func squashMiss(err error) error {
	fmt.Fprintf(os.Stderr, "layer squash skipped: %v\n", err)
	return nil
}

// ensureLocalBlob fetches one inherited layer blob from the parent
// repository when it is not already in the layout blob store.
func ensureLocalBlob(ctx context.Context, layout artifactLayout, repository string, desc ocispec.Descriptor) error {
	path := layout.blobPath(desc.Digest)
	if info, err := os.Stat(path); err == nil && info.Size() == desc.Size {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	credentials, err := packCredentials()
	if err != nil {
		return err
	}
	repo, _, err := ocitemplate.Repository(repository+"@"+desc.Digest.String(), credentials, ocitemplate.WithPlainHTTP(global.plainHTTP))
	if err != nil {
		return err
	}
	stream, err := repo.Blobs().Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := os.MkdirAll(layout.blobDir(), 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(layout.blobDir(), ".fetch-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	verifier := desc.Digest.Verifier()
	size, err := io.Copy(io.MultiWriter(temp, verifier), stream)
	if err == nil {
		err = temp.Close()
	}
	if err == nil && (size != desc.Size || !verifier.Verified()) {
		err = fmt.Errorf("blob %s: digest or size mismatch", desc.Digest)
	}
	if err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// packCredentials resolves pull credentials from the node's overlaybd
// credential file, like the runtime's own on-demand block reads.
func packCredentials() (ocitemplate.CredentialFunc, error) {
	config, err := readCredentials(global.credFile)
	if err != nil {
		return nil, err
	}
	return func(_ context.Context, reference string) (auth.Credential, error) {
		ref, err := parseRegistryReference(reference)
		if err != nil {
			return auth.EmptyCredential, err
		}
		credential, _, err := credentialFor(config, ref)
		return credential, err
	}, nil
}
