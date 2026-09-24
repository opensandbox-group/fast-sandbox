package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// stageBootAndSnapshot delegates to the builder image's snapshot stage
// helper (see snapshot_stage.go), which drives the firecracker API (machine
// config, boot source, InstanceStart, readiness wait, Pause, snapshot
// create, restore validation) and writes vmstate.snap and memory.snap. The
// returned phases carry the sub-phase durations reported by the helper.
func stageBootAndSnapshot(spec apiv1alpha2.SandboxTemplateSpec, workdir, kernel, rootfs string) (string, string, snapshotPhaseTimings, error) {
	vmstate := filepath.Join(workdir, "vmstate.snap")
	memory := filepath.Join(workdir, "memory.snap")
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", "", snapshotPhaseTimings{}, err
	}
	// The snapshot stage is the same binary invoked under its symlinked
	// name; use the current executable so local runs work without a staged
	// /usr/local/bin path.
	executable, err := os.Executable()
	if err != nil {
		return "", "", snapshotPhaseTimings{}, err
	}
	command := exec.Command(executable, "snapshot-stage", kernel, rootfs, vmstate, memory)
	command.Env = append(os.Environ(), specEnv+"="+string(specJSON))
	// Stream the stage's klog output to our own stderr so pipeline logs
	// show the sub-phase timings even on success; keep a copy for errors.
	var output bytes.Buffer
	command.Stdout = io.MultiWriter(os.Stdout, &output)
	command.Stderr = io.MultiWriter(os.Stderr, &output)
	if err := command.Run(); err != nil {
		return "", "", snapshotPhaseTimings{}, fmt.Errorf("snapshot stage: %w: %s", err, output.String())
	}
	var phases snapshotPhaseTimings
	payload, err := os.ReadFile(filepath.Join(workdir, "snapshot-phases.json"))
	if err != nil {
		return "", "", snapshotPhaseTimings{}, fmt.Errorf("snapshot stage report: %w", err)
	}
	if err := json.Unmarshal(payload, &phases); err != nil {
		return "", "", snapshotPhaseTimings{}, fmt.Errorf("decode snapshot stage report: %w", err)
	}
	// The report carries the CPU template the snapshot actually booted with;
	// an empty value would mislabel a template-masked snapshot as "none".
	if phases.CPUTemplate == "" {
		return "", "", snapshotPhaseTimings{}, errors.New("snapshot stage report carries no CPU template")
	}
	return vmstate, memory, phases, nil
}
