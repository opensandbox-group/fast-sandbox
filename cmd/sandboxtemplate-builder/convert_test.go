package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// TestWriteImageEnv: the pull stage persists the image's OCI Config.Env.
func TestWriteImageEnv(t *testing.T) {
	image, err := mutate.Config(empty.Image, v1.Config{Env: []string{"PATH=/usr/bin", "JAVA_HOME=/opt/java"}})
	if err != nil {
		t.Fatalf("mutate config: %v", err)
	}
	workdir := t.TempDir()
	if err := writeImageEnv(image, workdir); err != nil {
		t.Fatalf("writeImageEnv: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(workdir, imageEnvFileName))
	if err != nil {
		t.Fatalf("read staged env file: %v", err)
	}
	if got, want := string(payload), "PATH=/usr/bin\nJAVA_HOME=/opt/java\n"; got != want {
		t.Fatalf("staged env file = %q, want %q", got, want)
	}
}

// TestMergeGuestEnvs: image env inherited, spec env wins, spec envs strict.
func TestMergeGuestEnvs(t *testing.T) {
	tests := []struct {
		name      string
		imageEnvs string
		specEnvs  []corev1.EnvVar
		want      map[string]string
		wantError string
	}{
		{
			name:      "inherits image envs and lets spec envs override",
			imageEnvs: "PATH=/usr/bin\nLANG=C.UTF-8\n",
			specEnvs:  []corev1.EnvVar{{Name: "LANG", Value: "en_US.UTF-8"}, {Name: "EXTRA", Value: "1"}},
			want:      map[string]string{"PATH": "/usr/bin", "LANG": "en_US.UTF-8", "EXTRA": "1"},
		},
		{
			name:      "skips unusable image entries",
			imageEnvs: "GOOD=1\nnoequals\nBAD NAME=x\n",
			specEnvs:  nil,
			want:      map[string]string{"GOOD": "1"},
		},
		{
			name:      "missing image env file inherits nothing",
			imageEnvs: "",
			specEnvs:  []corev1.EnvVar{{Name: "ONLY", Value: "spec"}},
			want:      map[string]string{"ONLY": "spec"},
		},
		{
			name:      "empty image env value is kept",
			imageEnvs: "EMPTY=\n",
			specEnvs:  nil,
			want:      map[string]string{"EMPTY": ""},
		},
		{
			name:      "duplicate spec names resolve to the last entry",
			imageEnvs: "DUP=first\n",
			specEnvs:  []corev1.EnvVar{{Name: "DUP", Value: "a"}, {Name: "DUP", Value: "b"}},
			want:      map[string]string{"DUP": "b"},
		},
		{
			name:      "spec valueFrom fails the build",
			imageEnvs: "A=1\n",
			specEnvs:  []corev1.EnvVar{{Name: "B", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}},
			wantError: "valueFrom is not supported",
		},
		{
			name:      "invalid spec env name fails the build",
			imageEnvs: "A=1\n",
			specEnvs:  []corev1.EnvVar{{Name: "BAD NAME", Value: "x"}},
			wantError: "not a valid shell variable name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workdir := t.TempDir()
			if test.imageEnvs != "" {
				if err := os.WriteFile(filepath.Join(workdir, imageEnvFileName), []byte(test.imageEnvs), 0o644); err != nil {
					t.Fatalf("stage image env file: %v", err)
				}
			}
			merged, err := mergeGuestEnvs(apiv1alpha2.SandboxTemplateSpec{Envs: test.specEnvs}, workdir)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("mergeGuestEnvs error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("mergeGuestEnvs: %v", err)
			}
			if !reflect.DeepEqual(merged, test.want) {
				t.Fatalf("merged = %v, want %v", merged, test.want)
			}
		})
	}
}

// TestReadImageEnvsMissingFileIsNoInheritance: missing file, no failure.
func TestReadImageEnvsMissingFileIsNoInheritance(t *testing.T) {
	envs, err := readImageEnvs(t.TempDir())
	if err != nil {
		t.Fatalf("readImageEnvs: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("envs = %v, want empty", envs)
	}
}

// TestMergeGuestEnvsNamesAreSorted: deterministic byte-identical rendering.
func TestMergeGuestEnvsNamesAreSorted(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, imageEnvFileName),
		[]byte("ZEBRA=1\nALPHA=2\n"), 0o644); err != nil {
		t.Fatalf("stage image env file: %v", err)
	}
	merged, err := mergeGuestEnvs(apiv1alpha2.SandboxTemplateSpec{}, workdir)
	if err != nil {
		t.Fatalf("mergeGuestEnvs: %v", err)
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	if !sort.StringsAreSorted(names) {
		t.Fatalf("names not sorted: %v", names)
	}
}

// TestRenderGuestInitLoopbackGate: when the readiness gate dials loopback
// (execd ping, or a tcp://127.* probe), an image without ip/ifconfig must
// fail loudly instead of silently running down the readiness timeout;
// otherwise loopback setup stays best-effort so warmup-only templates keep
// building on such images.
func TestRenderGuestInitLoopbackGate(t *testing.T) {
	tests := []struct {
		name    string
		spec    apiv1alpha2.SandboxTemplateSpec
		wantSub string
		wantNot string
	}{
		{
			name:    "execd gate requires loopback",
			spec:    apiv1alpha2.SandboxTemplateSpec{Execd: "opensandbox/execd:1.1.0"},
			wantSub: `echo "SANDBOX_STARTUP_FAILED no_loopback_tool"`,
		},
		{
			name:    "loopback probe requires loopback",
			spec:    apiv1alpha2.SandboxTemplateSpec{Readiness: apiv1alpha2.ReadinessSpec{Probe: "tcp://127.0.0.1:44772"}},
			wantSub: `echo "SANDBOX_STARTUP_FAILED no_loopback_tool"`,
		},
		{
			name:    "warmup-only readiness keeps loopback best-effort",
			spec:    apiv1alpha2.SandboxTemplateSpec{},
			wantSub: `echo "loopback setup skipped`,
			wantNot: `echo "SANDBOX_STARTUP_FAILED no_loopback_tool"`,
		},
		{
			name:    "remote probe keeps loopback best-effort",
			spec:    apiv1alpha2.SandboxTemplateSpec{Readiness: apiv1alpha2.ReadinessSpec{Probe: "tcp://10.0.0.1:80"}},
			wantSub: `echo "loopback setup skipped`,
			wantNot: `echo "SANDBOX_STARTUP_FAILED no_loopback_tool"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := renderGuestInit(test.spec)
			if !strings.Contains(script, "ifconfig lo up 2>/dev/null") {
				t.Fatalf("init lacks the ifconfig fallback for loopback setup")
			}
			if !strings.Contains(script, guestBusyboxPath+" ip link set lo up") {
				t.Fatalf("init lacks the injected-busybox fallback for loopback setup")
			}
			if !strings.Contains(script, test.wantSub) {
				t.Fatalf("init does not contain %q", test.wantSub)
			}
			if test.wantNot != "" && strings.Contains(script, test.wantNot) {
				t.Fatalf("init must not contain %q for this readiness gate", test.wantNot)
			}
		})
	}
}

// TestInjectBusyboxFrom: the builder's static busybox lands at the path the
// init's fallback chain references; no readable candidate reports false.
func TestInjectBusyboxFrom(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "busybox")
	if err := os.WriteFile(source, []byte("#!elf"), 0o755); err != nil {
		t.Fatalf("stage busybox source: %v", err)
	}
	rootfs := t.TempDir()
	if !injectBusyboxFrom([]string{filepath.Join(workdir, "missing"), source}, rootfs) {
		t.Fatalf("injectBusyboxFrom reported no injection for an existing candidate")
	}
	payload, err := os.ReadFile(filepath.Join(rootfs, guestBusyboxPath))
	if err != nil {
		t.Fatalf("injected busybox missing: %v", err)
	}
	if string(payload) != "#!elf" {
		t.Fatalf("injected busybox = %q, want the source payload", payload)
	}
	if injectBusyboxFrom([]string{filepath.Join(workdir, "missing")}, rootfs) {
		t.Fatalf("injectBusyboxFrom reported injection without a readable candidate")
	}
}

// Execute the rendered readiness shell with an isolated PATH so host tools
// cannot hide a missing guest dependency. No VM or privileged mounts needed.
func TestGuestInitTCPReadinessTools(t *testing.T) {
	tests := []struct {
		name, probe, wantTool                string
		busybox, imageNC, execd, wantFailure bool
	}{
		{name: "tcp uses injected busybox", probe: "tcp://127.0.0.1:8080", busybox: true, wantTool: "busybox"},
		{name: "injected busybox takes precedence", probe: "tcp://127.0.0.1:8080", busybox: true, imageNC: true, wantTool: "busybox"},
		{name: "execd uses injected busybox", busybox: true, execd: true, wantTool: "busybox"},
		{name: "tcp falls back to image nc", probe: "tcp://127.0.0.1:8080", imageNC: true, wantTool: "image"},
		{name: "tcp fails without either tool", probe: "tcp://127.0.0.1:8080", wantFailure: true},
		{name: "execd fails without either tool", execd: true, wantFailure: true},
		{name: "command probe needs no nc", probe: "cmd://true", execd: true},
		{name: "warmup needs no nc"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeExecutable := func(name, body string) string {
				t.Helper()
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
				return path
			}
			// Model an injected BusyBox that lacks the nc applet by default.
			busybox := writeExecutable("busybox", "exit 1\n")
			if test.busybox {
				writeExecutable("busybox", `
[ "$1" = nc ] || exit 1
shift
[ "$1" = --help ] && exit 0
printf 'busybox %s\n' "$*" >> "$CALLS"
`)
			}
			if test.imageNC {
				writeExecutable("nc", `printf 'image %s\n' "$*" >> "$CALLS"`)
			}
			writeExecutable("sleep", "exit 0\n")
			execd := filepath.Join(dir, "execd")
			if test.execd {
				writeExecutable("execd", "exit 0\n")
			}
			script := renderGuestInit(apiv1alpha2.SandboxTemplateSpec{})
			start := strings.Index(script, "select_nc() {")
			end := strings.Index(script, "echo SANDBOX_READY") + len("echo SANDBOX_READY")
			if start < 0 || end < start {
				t.Fatal("rendered readiness block missing")
			}
			script = strings.ReplaceAll(script[start:end], guestBusyboxPath, shellQuote(busybox))
			script = strings.ReplaceAll(script, "/opt/opensandbox/execd", execd)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-eu", "-c", script)
			calls := filepath.Join(dir, "calls")
			cmd.Env = []string{"PATH=" + dir, "CALLS=" + calls, "READINESS_PROBE=" + test.probe, "WARMUP_SECONDS=0"}
			output, err := cmd.CombinedOutput()
			if test.wantFailure {
				if err == nil || !strings.Contains(string(output), "SANDBOX_STARTUP_FAILED nc_missing") || strings.Contains(string(output), "SANDBOX_READY") {
					t.Fatalf("expected nc_missing failure, got %v: %s", err, output)
				}
				return
			}
			if err != nil || !strings.Contains(string(output), "SANDBOX_READY") {
				t.Fatalf("readiness failed: %v: %s", err, output)
			}
			payload, err := os.ReadFile(calls)
			if test.wantTool == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("unexpected TCP check: %q, %v", payload, err)
				}
				return
			}
			port := "8080"
			if test.execd {
				port = "44772"
			}
			want := test.wantTool + " -w 1 127.0.0.1 " + port + "\n"
			if err != nil || string(payload) != want {
				t.Fatalf("TCP calls = %q, %v; want %q", payload, err, want)
			}
		})
	}
}
