package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// TestWriteImageEnv verifies the pull stage persists the source image's OCI
// Config.Env verbatim for the convert and manifest stages.
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

// TestMergeGuestEnvs verifies the guest env merge: the source image's
// Config.Env is inherited, spec envs win on name conflicts, unusable image
// entries are skipped, and spec envs stay strict.
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

// TestReadImageEnvsMissingFileIsNoInheritance verifies a workdir without the
// pull-stage env file does not fail the convert stage.
func TestReadImageEnvsMissingFileIsNoInheritance(t *testing.T) {
	envs, err := readImageEnvs(t.TempDir())
	if err != nil {
		t.Fatalf("readImageEnvs: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("envs = %v, want empty", envs)
	}
}

// TestMergeGuestEnvsNamesAreSorted verifies the merge output renders in
// sorted name order so builds of the same inputs stay byte-identical
// (injectRuntime sorts the names before rendering the exports).
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
