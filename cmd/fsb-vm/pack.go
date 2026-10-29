package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/ocitemplate"
)

// pack exports an already captured checkpoint. It never boots a new VM.
func newPackCommand() *cobra.Command {
	var bundle, output string
	cmd := &cobra.Command{Use: "pack", Short: "Package captured rootfs, memory and VM state as an OverlayBD pair", RunE: func(cmd *cobra.Command, _ []string) error { return packCheckpoint(cmd.Context(), bundle, output) }}
	cmd.Flags().StringVar(&bundle, "bundle-dir", "", "captured checkpoint directory")
	cmd.Flags().StringVar(&output, "output", "", "local logical output name:tag")
	_ = cmd.MarkFlagRequired("bundle-dir")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func packCheckpoint(ctx context.Context, bundle, outputName string) (resultErr error) {
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
	rootLayers, err := packRootfs(ctx, bundle, work, layout, upper)
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

func packRootfs(ctx context.Context, bundle, work string, layout artifactLayout, upper *ocitemplate.RootfsUpper) ([]ocispec.Descriptor, error) {
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
	// A no-write checkpoint commits a byte-identical layer; keep the
	// inherited stack so the rootfs layer chain stays unchanged.
	if len(rootLayers) > 0 && rootLayers[len(rootLayers)-1].Digest == root.Digest {
		return rootLayers, nil
	}
	rootLayers = append(rootLayers, root)

	return rootLayers, nil
}
