package main

// oci_publish_cmd.go implements the `oci-publish` subcommand: a standalone
// entry into the OverlayBD OCI image packaging stage, decoupled from the
// full golden-image pipeline. It takes existing rootfs/memory raw images,
// packs each as a single-layer OverlayBD OCI image via strmvold
// (attach empty raw volume → byte-exact write → commit → push), and prints
// the digest-pinned references.
//
// This is the seam scripts/sandboxtemplate-oci-e2e.sh exercises: no KVM,
// no Firecracker, no S3 — just the strmvold integration against a registry.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// runOCIPublish parses the oci-publish flags and drives
// stagePublishOCIImages with a synthetic spec.
func runOCIPublish(args []string) error {
	flags := flag.NewFlagSet("oci-publish", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var (
		rootfsPath = flags.String("rootfs", "", "path to the raw rootfs image (e.g. rootfs.ext4); required")
		memoryPath = flags.String("memory", "", "path to the raw memory snapshot (e.g. memory.snap); required")
		registry   = flags.String("registry", "", "OCI registry base ref, e.g. registry.example.com/fs-templates/t1; required")
		tag        = flags.String("tag", "", "image tag for both derived refs (default: short workspace id)")
		rootfsGiB  = flags.Int("rootfs-size-gib", 0, "rootfs volume size in GiB (default: ceil of the file size)")
		memoryGiB  = flags.Int("memory-size-gib", 0, "memory volume size in GiB (default: ceil of the file size)")
		workdir    = flags.String("workdir", "", "build workspace for the strmvold session (default: temp dir)")
		noPush     = flags.Bool("no-push", false, "commit only: keep manifest + layer blob under the strmvold session root, do not upload")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *rootfsPath == "" || *memoryPath == "" || *registry == "" {
		return fmt.Errorf("--rootfs, --memory and --registry are required")
	}
	for _, path := range []string{*rootfsPath, *memoryPath} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
	}

	keepWorkdir := *noPush
	if *workdir == "" {
		dir, err := os.MkdirTemp("", "oci-publish-*")
		if err != nil {
			return err
		}
		*workdir = dir
		if !keepWorkdir {
			defer os.RemoveAll(dir)
		}
	}
	if err := os.MkdirAll(*workdir, 0o750); err != nil {
		return err
	}

	spec := apiv1alpha2.SandboxTemplateSpec{Output: apiv1alpha2.OutputSpec{
		Registry:   *registry,
		RootfsSize: fmt.Sprintf("%dGi", ceilGiBFlag(*rootfsGiB, *rootfsPath)),
	}, Machine: apiv1alpha2.MachineSpec{
		Memory: fmt.Sprintf("%dGi", ceilGiBFlag(*memoryGiB, *memoryPath)),
	}}

	refs, err := stagePublishOCIImages(context.Background(), spec, *workdir, *rootfsPath, *memoryPath, *tag, *noPush)
	if err != nil {
		return err
	}
	fmt.Printf("rootfs-ref: %s\n", refs.Rootfs)
	fmt.Printf("memory-ref: %s\n", refs.Memory)
	serviceRoot := filepath.Join(*workdir, "strmvol")
	for _, entry := range []struct{ name, target string }{
		{"rootfs", strings.Split(refs.Rootfs, "@")[0]},
		{"memory", strings.Split(refs.Memory, "@")[0]},
	} {
		artifacts, err := ociResolveLocalArtifacts(serviceRoot, entry.target)
		if err != nil {
			return fmt.Errorf("resolve %s local artifacts: %w", entry.name, err)
		}
		fmt.Printf("%s-manifest: %s\n", entry.name, artifacts.ManifestPath)
		fmt.Printf("%s-blob: %s\n", entry.name, artifacts.BlobPath)
	}
	return nil
}

// ceilGiBFlag returns the explicit GiB size, or the file's size rounded up
// to whole GiB (the volume only needs to fit the image).
func ceilGiBFlag(configured int, path string) int {
	if configured > 0 {
		return configured
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return 1
	}
	gib := (int64(info.Size()) + (1 << 30) - 1) >> 30
	if gib < 1 {
		gib = 1
	}
	return int(gib)
}
