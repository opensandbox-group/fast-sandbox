package oci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFastletImageBuildDoesNotRequireHostCompilers(t *testing.T) {
	root := t.TempDir()
	makefile, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "Makefile"), makefile, 0600))
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TEST_IMAGE_LOG\"\n"), 0755))
	log := filepath.Join(root, "commands")
	cmd := exec.Command("make", "images", "COMPONENT=fastlet", "GO="+filepath.Join(bin, "missing-go"), "DEBUG=1", "FASTLET_IMAGE=opensandbox/fsb-fastlet:test")
	cmd.Dir = root
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GOPROXY=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_IMAGE_LOG="+log)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	raw, err := os.ReadFile(log)
	require.NoError(t, err)
	require.Equal(t, "build\n--platform\nlinux/amd64\n--build-arg\nGOPROXY=https://proxy.golang.org,direct\n--build-arg\nDEBUG=1\n-t\nopensandbox/fsb-fastlet:test\n-f\nbuild/Dockerfile.fastlet\n.\n", string(raw))
	require.NoDirExists(t, filepath.Join(root, ".build"))
}

func TestSandboxTemplateBuilderCopiesComponentLogging(t *testing.T) {
	dockerfile, err := os.ReadFile("../Dockerfile.sandboxtemplate-builder")
	require.NoError(t, err)
	_, stage, found := strings.Cut(string(dockerfile), " AS builder-build\n")
	require.True(t, found)
	stage, _, found = strings.Cut(stage, "\nFROM ")
	require.True(t, found)
	beforeBuild, _, found := strings.Cut(stage, "RUN CGO_ENABLED=0 go build")
	require.True(t, found)
	require.Contains(t, beforeBuild, "\nCOPY internal/componentlog ./internal/componentlog\n")
}

func TestSharedOCIToolImageBuildSelection(t *testing.T) {
	for _, test := range []struct {
		name, component, image string
		builds                 int
	}{
		{name: "tools-without-host-go", component: "oci-tools", builds: 1},
		{name: "builder-builds-local-tools", component: "sandboxtemplate-builder", builds: 2},
		{name: "builder-reuses-pinned-tools", component: "sandboxtemplate-builder", image: "r.example/oci-tools@sha256:" + strings.Repeat("a", 64), builds: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			makefile, err := os.ReadFile("../../Makefile")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, "Makefile"), makefile, 0600))
			bin := filepath.Join(root, "bin")
			require.NoError(t, os.MkdirAll(bin, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_IMAGE_LOG\"\n"), 0755))
			log := filepath.Join(root, "commands")
			args := []string{"images", "COMPONENT=" + test.component, "REGISTRY=fast-sandbox", "GO=" + filepath.Join(bin, "missing-go")}
			image := "fast-sandbox/oci-tools:dev"
			if test.image != "" {
				image = test.image
			}
			args = append(args, "OCI_TOOLS_IMAGE="+image)
			command := exec.Command("make", args...)
			command.Dir = root
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_IMAGE_LOG="+log)
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
			raw, err := os.ReadFile(log)
			require.NoError(t, err)
			calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
			require.Len(t, calls, test.builds)
			if test.component == "oci-tools" || test.builds == 2 {
				require.Contains(t, calls[0], "-f build/Dockerfile.oci-tools")
				require.Contains(t, calls[0], "--platform linux/amd64")
			}
			if test.component == "sandboxtemplate-builder" {
				require.Contains(t, calls[len(calls)-1], "--build-arg OCI_TOOLS_IMAGE="+image)
				require.Contains(t, calls[len(calls)-1], "-f build/Dockerfile.sandboxtemplate-builder")
			}
			require.NoDirExists(t, filepath.Join(root, ".build"))
		})
	}
}
