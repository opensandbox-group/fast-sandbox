// Command sandboxtemplate-builder executes the golden-image build pipeline
// described by a SandboxTemplate spec (sandbox.fast.io/v1alpha2).
//
// The spec arrives serialized in the SANDBOX_TEMPLATE_SPEC environment
// variable, injected by the controller when it creates the build Pod. The
// builder runs inside a privileged Pod with /dev/kvm and shells out to the
// platform toolchain (oci2rootfs, firecracker, e2fsprogs,
// fsb-vm, aws CLI). On success it patches its own Pod
// annotations so the controller can surface manifestRef and artifactDigest
// on the template status.
//
// The pipeline is organized in per-stage files:
//
//	oci.go      — pull the source image into an OCI layout (+ execd extraction)
//	convert.go  — materialize the layout into a sparse ext4 rootfs and inject the runtime
//	snapshot.go — drive firecracker to boot, snapshot, and restore-validate
//	manifest.go — assemble manifest.json and SHA256SUMS
//	publish.go  — upload artifacts and report the outcome on the Pod
//	oci_template.go — build and publish OverlayBD OCI images through fsb-vm
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
	"strings"
	"time"

	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

const (
	specEnv                  = "SANDBOX_TEMPLATE_SPEC"
	workDirEnv               = "SANDBOX_TEMPLATE_WORKDIR"
	publishCredentialsDirEnv = "SANDBOX_TEMPLATE_PUBLISH_SECRET_DIR"
	manifestRefAnnotation    = "sandbox.fast.io/manifest-ref"
	digestAnnotation         = "sandbox.fast.io/artifact-digest"
)

// Tool paths inside the builder image.
const (
	oci2rootfsBin  = "oci2rootfs"
	firecrackerBin = "firecracker"
	awsBin         = "aws"
	kernelDir      = "/usr/local/share/firecracker"
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
	klog.InitFlags(nil)
	// Parse flags so klog's -v/-logtostderr work on the command line.
	flag.Parse()
	if err := run(context.Background()); err != nil {
		klog.ErrorS(err, "sandbox template build failed")
		os.Exit(1)
	}
}

// run selects the build pipeline by publication destination.
func run(ctx context.Context) error {
	spec, workdir, err := loadSpecAndWorkdir()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(spec.Output.Publish, "oci://") && spec.Output.Format != apiv1alpha2.ArtifactFormatNative {
		return fmt.Errorf("object-store templates require output.format=native; overlaybd requires output.publish=oci://...")
	}
	kernel, err := resolveKernel(spec.Kernel)
	if err != nil {
		return err
	}
	if strings.HasPrefix(spec.Output.Publish, "oci://") {
		return buildOCITemplate(ctx, spec, workdir, kernel)
	}
	return buildObjectStoreTemplate(ctx, spec, workdir, kernel)
}

// buildObjectStoreTemplate retains the local OCI layout / oci2rootfs pipeline.
func buildObjectStoreTemplate(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir, kernel string) error {
	credentials, err := loadPublishCredentials(os.Getenv(publishCredentialsDirEnv))
	if err != nil {
		return err
	}
	registryAuth, err := loadRegistryAuth(os.Getenv(registryConfigPathEnv))
	if err != nil {
		return err
	}
	started := time.Now()

	// Stage 1: convert — OCI image → sparse ext4 rootfs with the runtime injected.
	convertStarted := time.Now()
	sourceDigest, err := pullOCILayout(ctx, spec, workdir, registryAuth)
	if err != nil {
		return err
	}
	pullMs := time.Since(convertStarted).Milliseconds()
	convertStarted = time.Now()
	rootfs, mitmCA, err := stageConvert(spec, workdir)
	if err != nil {
		return err
	}
	convertMs := time.Since(convertStarted).Milliseconds()

	// Stage 2+3: validate-boot and snapshot.
	bootStarted := time.Now()
	vmstate, memory, phases, err := stageBootAndSnapshot(spec, workdir, kernel, rootfs)
	if err != nil {
		return err
	}
	bootMs := time.Since(bootStarted).Milliseconds()

	// Stage 4: manifest + checksums, recording the effective CPU template
	// and the guest FPR support detected by the snapshot stage.
	manifestStarted := time.Now()
	manifestBytes, err := stageManifest(spec, sourceDigest, kernel, rootfs, vmstate, memory, workdir, phases.CPUTemplate, phases.FreePageReporting, mitmCA)
	if err != nil {
		return err
	}
	manifestMs := time.Since(manifestStarted).Milliseconds()

	// Stage 5: publish to the object store; the manifest is uploaded last so
	// consumers never observe a half-published artifact set.
	publishMs := int64(0)
	manifestRef := ""
	if spec.Output.Publish != "" {
		publishStarted := time.Now()
		manifestRef, err = publish(ctx, spec, workdir, manifestBytes, credentials)
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
		"manifestMs", manifestMs,
		"publishMs", publishMs,
		"totalMs", time.Since(started).Milliseconds())

	return patchPodAnnotations(ctx, manifestRef, sha256Of(manifestBytes))
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
