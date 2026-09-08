// Command sandboxtemplate-builder executes the golden-image build pipeline
// described by a SandboxTemplate spec (sandbox.fast.io/v1alpha2).
//
// The spec arrives serialized in the SANDBOX_TEMPLATE_SPEC environment
// variable, injected by the controller when it creates the build Pod. The
// builder runs inside a privileged Pod with /dev/kvm and shells out to the
// platform toolchain (oci2rootfs, firecracker, e2fsprogs,
// overlaybd-import-raw, aws CLI). On success it patches its own Pod
// annotations so the controller can surface manifestRef and artifactDigest
// on the template status.
//
// The pipeline is organized in per-stage files mirroring the design doc:
//
//	oci.go      — pull the source image into an OCI layout (+ execd extraction)
//	convert.go  — materialize the layout into a sparse ext4 rootfs and inject the runtime
//	snapshot.go — drive firecracker to boot, snapshot, and restore-validate
//	package.go  — encode the snapshot artifacts as OverlayBD layers
//	manifest.go — assemble manifest.json and SHA256SUMS
//	publish.go  — upload artifacts and report the outcome on the Pod
//
// In addition, the builder image ships the same binary under the symlinked
// name sandbox-snapshot-stage; that entry point runs the snapshot helper
// (see snapshot_stage.go) instead of the pipeline.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	"k8s.io/klog/v2"
)

const (
	specEnv                     = "SANDBOX_TEMPLATE_SPEC"
	workDirEnv                  = "SANDBOX_TEMPLATE_WORKDIR"
	manifestRefAnnotation       = "sandbox.fast.io/manifest-ref"
	digestAnnotation            = "sandbox.fast.io/artifact-digest"
	rootfsImageRefAnnotation    = "sandbox.fast.io/rootfs-image-ref"
	memoryImageRefAnnotation    = "sandbox.fast.io/memory-image-ref"
)

// Tool paths inside the builder image.
const (
	oci2rootfsBin      = "oci2rootfs"
	firecrackerBin     = "firecracker"
	overlaybdImportBin = "overlaybd-import-raw"
	awsBin             = "aws"
	kernelDir          = "/usr/local/share/firecracker"
)

func main() {
	// Two ways to reach the snapshot stage entry: the symlinked binary name
	// (builder image) or the explicit subcommand (local runs).
	if filepath.Base(os.Args[0]) == "sandbox-snapshot-stage" {
		if err := runSnapshotStage(os.Args[1:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "snapshot stage failed:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "snapshot-stage" {
		if err := runSnapshotStage(os.Args[2:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "snapshot stage failed:", err)
			os.Exit(1)
		}
		return
	}
	// oci-publish: standalone OverlayBD OCI image packaging (see
	// oci_publish_cmd.go); exercised by scripts/sandboxtemplate-oci-e2e.sh.
	if len(os.Args) > 1 && os.Args[1] == "oci-publish" {
		if err := runOCIPublish(os.Args[2:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "oci-publish failed:", err)
			os.Exit(1)
		}
		return
	}
	klog.InitFlags(nil)
	// Parse flags so klog's -v/-logtostderr work on the command line.
	flag.Parse()
	if err := run(context.Background()); err != nil {
		klog.ErrorS(err, "sandbox template build failed")
		os.Exit(1)
	}
}

// run executes the build pipeline and logs the per-stage durations.
func run(ctx context.Context) error {
	spec, workdir, err := loadSpecAndWorkdir()
	if err != nil {
		return err
	}
	kernel, err := resolveKernel(spec.Kernel)
	if err != nil {
		return err
	}
	started := time.Now()

	// Stage 1: convert — OCI image → sparse ext4 rootfs with the runtime injected.
	convertStarted := time.Now()
	sourceDigest, err := pullOCILayout(ctx, spec, workdir)
	if err != nil {
		return err
	}
	pullMs := time.Since(convertStarted).Milliseconds()
	convertStarted = time.Now()
	rootfs, err := stageConvert(spec, workdir)
	if err != nil {
		return err
	}
	convertMs := time.Since(convertStarted).Milliseconds()

	// Stage 2+3: validate-boot and snapshot (mandatory for every format).
	bootStarted := time.Now()
	vmstate, memory, phases, err := stageBootAndSnapshot(spec, workdir, kernel, rootfs)
	if err != nil {
		return err
	}
	bootMs := time.Since(bootStarted).Milliseconds()

	// Stage 4: package — OverlayBD encoding (overlaybd format only). With an
	// output registry the OCI image path replaces the legacy import-raw
	// packaging: rootfs and memory are published as single-layer OverlayBD
	// OCI images and never touch S3 as large objects.
	var layers []string
	var imageRefs ociImageRefs
	importRootfsMs, importMemoryMs := int64(0), int64(0)
	ociPublishMs := int64(0)
		if spec.Output.Format == apiv1alpha2.ArtifactFormatOverlayBD {
			if spec.Output.Registry != "" {
				ociStarted := time.Now()
				imageRefs, err = stagePublishOCIImages(ctx, spec, workdir, rootfs, memory, ociImageTag(buildShortID(workdir)), false)
			if err != nil {
				return err
			}
			ociPublishMs = time.Since(ociStarted).Milliseconds()
		} else {
			layers, importRootfsMs, importMemoryMs, err = stagePackage(workdir, rootfs, memory)
			if err != nil {
				return err
			}
		}
	}

	// Stage 5: manifest + checksums.
	manifestStarted := time.Now()
	manifestBytes, err := stageManifest(spec, sourceDigest, kernel, rootfs, vmstate, memory, layers, imageRefs, workdir)
	if err != nil {
		return err
	}
	manifestMs := time.Since(manifestStarted).Milliseconds()

	// Stage 6: publish the S3-side artifacts; the manifest is uploaded last
	// so consumers never observe a half-published artifact set.
	publishMs := int64(0)
	manifestRef := ""
	if spec.Output.Publish != "" {
		publishStarted := time.Now()
		manifestRef, err = publish(ctx, spec, workdir, manifestBytes, imageRefs)
		if err != nil {
			return err
		}
		publishMs = time.Since(publishStarted).Milliseconds()
	} else if os.Getenv("SANDBOX_TEMPLATE_ALLOW_NO_PUBLISH") == "" {
		// The CRD makes publish required, but the builder can run without
		// admission (E2E, local): without a publish target the artifacts
		// only exist in the ephemeral workspace, so fail loudly instead of
		// letting the controller mark a hollow build Succeeded.
		return errors.New("output.publish is required (set SANDBOX_TEMPLATE_ALLOW_NO_PUBLISH=1 to skip publishing)")
	}

	klog.InfoS("sandbox template build stages",
		"format", spec.Output.Format,
		"pullMs", pullMs,
		"convertMs", convertMs,
		"bootToReadyMs", phases.BootToReadyMs,
		"snapshotCreateMs", phases.SnapshotCreateMs,
		"restoreToHeartbeatMs", phases.RestoreToHeartbeatMs,
		"bootMs", bootMs,
		"importRootfsMs", importRootfsMs,
		"importMemoryMs", importMemoryMs,
		"ociPublishMs", ociPublishMs,
		"manifestMs", manifestMs,
		"publishMs", publishMs,
		"totalMs", time.Since(started).Milliseconds())

	return patchPodAnnotations(ctx, manifestRef, sha256Of(manifestBytes), imageRefs)
}

// loadSpecAndWorkdir reads the serialized SandboxTemplateSpec (the controller
// injects the spec, not the wrapping CR; the E2E script feeds the same shape)
// and prepares the build workspace.
func loadSpecAndWorkdir() (apiv1alpha2.SandboxTemplateSpec, string, error) {
	payload := os.Getenv(specEnv)
	if payload == "" {
		return apiv1alpha2.SandboxTemplateSpec{}, "", errors.New("SANDBOX_TEMPLATE_SPEC is required")
	}
	var spec apiv1alpha2.SandboxTemplateSpec
	if err := json.Unmarshal([]byte(payload), &spec); err != nil {
		return apiv1alpha2.SandboxTemplateSpec{}, "", fmt.Errorf("parse template spec: %w", err)
	}
	workdir := os.Getenv(workDirEnv)
	if workdir == "" {
		workdir = "/build"
	}
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		return apiv1alpha2.SandboxTemplateSpec{}, "", err
	}
	return spec, workdir, nil
}
