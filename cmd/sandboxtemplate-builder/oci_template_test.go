package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/componentlog"
	"fast-sandbox/internal/guestnetwork"
)

func TestOCIBuildUsesRemoteSourceWithoutDownloadingSourceLayers(t *testing.T) {
	for _, test := range []struct {
		private bool
		kernel  string
		version string
		failure string
		wantErr string
	}{
		{false, "/kernels/vmlinux-6.1.0", "6.1.0", "", ""},
		{true, "/kernels/vmlinux-6.1.0", "6.1.0", "", ""},
		{false, "/kernels/vmlinux.bin", "vmlinux.bin", "", ""},
		{true, "/kernels/vmlinux.bin", "vmlinux.bin", "", ""},
		{true, "/kernels/vmlinux-6.1.0", "6.1.0", "build", "fsb-vm build: exit status 8"},
		{true, "/kernels/vmlinux-6.1.0", "6.1.0", "push", "fsb-vm push: exit status 9"},
		{true, "/kernels/vmlinux-6.1.0", "6.1.0", "invalid-json", "invalid character 'i' looking for beginning of value"},
		{true, "/kernels/vmlinux-6.1.0", "6.1.0", "incomplete-json", "fsb-vm push returned incomplete publication"},
	} {
		testName := map[bool]string{false: "anonymous", true: "basic-auth"}[test.private] + "/" + filepath.Base(test.kernel)
		if test.failure != "" {
			testName += "/" + test.failure
		}
		t.Run(testName, func(t *testing.T) {
			private := test.private
			dir := t.TempDir()
			t.Setenv("DOCKER_CONFIG", dir)
			t.Setenv("SANDBOX_TEMPLATE_IMAGE_TAR", "")
			t.Setenv("KUBERNETES_SERVICE_HOST", "") // Standalone annotation reporting.
			amd64 := ociTestImage(t, "amd64")
			arm64 := ociTestImage(t, "arm64")
			index := mutate.AppendManifests(empty.Index,
				mutate.IndexAddendum{Add: arm64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
				mutate.IndexAddendum{Add: amd64, Descriptor: v1.Descriptor{Platform: &linuxAMD64}},
			)
			layers, err := amd64.Layers()
			require.NoError(t, err)
			layerDigest, err := layers[0].Digest()
			require.NoError(t, err)
			var sourceLayerReads atomic.Int32
			handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if private {
					user, password, ok := r.BasicAuth()
					if !ok || user != "reader" || password != "secret" {
						w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v2/base/blobs/"+layerDigest.String() {
					sourceLayerReads.Add(1)
					http.Error(w, "builder must not download source layers", http.StatusInternalServerError)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			tag, err := name.NewTag(host + "/base:mutable")
			require.NoError(t, err)
			credential := authn.Anonymous
			config := map[string]any{"auths": map[string]any{}}
			if private {
				credential = authn.FromConfig(authn.AuthConfig{Username: "reader", Password: "secret"})
				config["auths"] = map[string]any{host: authn.AuthConfig{Username: "reader", Password: "secret"}}
			}
			require.NoError(t, remote.WriteIndex(tag, index, remote.WithAuth(credential)))
			execdTag, err := name.NewTag(host + "/execd:build")
			require.NoError(t, err)
			require.NoError(t, remote.Write(execdTag, ociTestExecd(t), remote.WithAuth(credential)))
			raw, err := json.Marshal(config)
			require.NoError(t, err)
			t.Setenv(publishCredentialsDirEnv, dir)
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerconfigjson"), raw, 0600))

			bin := t.TempDir()
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_OCI_COMMANDS", dir)
			// Guest control is unconditional on OverlayBD builds: the test
			// builder image ships the helper through this override.
			t.Setenv("FSB_GUEST_CONTROL_HELPER", staticHelperBinary(t))
			buildID := uuid.NewString()
			t.Setenv(buildIDEnvName, buildID)
			require.NoError(t, os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_OCI_COMMANDS/ip.log\"\n"), 0755))
			t.Setenv("TEST_OCI_FAILURE", test.failure)
			require.NoError(t, os.WriteFile(filepath.Join(bin, "fsb-vm"), []byte(`#!/bin/sh
if [ "$1" = push ]; then
  printf '%s\n' "$@" > "$TEST_OCI_COMMANDS/push-args"
  printf 'push stderr\n' >&2
  case "$TEST_OCI_FAILURE" in
    push) exit 9 ;;
    invalid-json) printf 'invalid JSON\n'; exit 0 ;;
    incomplete-json) printf '{}\n'; exit 0 ;;
  esac
  printf '%s\n' '{"manifestRef":"oci://registry.example.com/template@sha256:abc","artifactDigest":"sha256:abc"}'
  exit 0
fi
printf '%s\n' "$@" > "$TEST_OCI_COMMANDS/fsb-args"
printf 'build stdout\n'
printf 'build stderr\n' >&2
if [ "$TEST_OCI_FAILURE" = build ]; then
  exit 8
fi
`), 0755))
			spec := apiv1alpha2.SandboxTemplateSpec{
				Image: tag.String(), Execd: execdTag.String(),
				Machine: apiv1alpha2.MachineSpec{VCPU: "1", Memory: "512Mi"},
				Envs:    []corev1.EnvVar{{Name: "OVERRIDE", Value: "from-spec"}},
				Output:  apiv1alpha2.OutputSpec{Format: apiv1alpha2.ArtifactFormatOverlayBD, RootfsSize: "1Gi", Publish: "oci://" + host + "/template"},
			}
			spec.Readiness.WarmupSeconds = 600
			stdout, stderr := captureOCIChildOutput(t)
			readLogs := captureOCIBuildLogs(t)
			err = buildOCITemplate(context.Background(), spec, dir, test.kernel)
			logs := readLogs()
			failedPhase := ""
			if test.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, test.wantErr)
				require.Contains(t, logs, fmt.Sprintf("err=%q", err.Error()))
				failedPhase = "push"
				if test.failure == "build" {
					failedPhase = "build"
				}
			}
			requireOCIBuildPhases(t, logs, failedPhase)
			for _, sensitive := range []string{"secret", "--auth-file", "--source", "--destination"} {
				require.NotContains(t, logs, sensitive)
			}
			for _, marker := range []string{"build stdout", "build stderr", "push stderr", "manifestRef"} {
				require.NotContains(t, logs, marker, "child output must not be redirected to the component log")
			}
			raw, err = os.ReadFile(stdout.Name())
			require.NoError(t, err)
			require.Equal(t, "build stdout\n", string(raw), "push JSON must remain captured, not streamed")
			raw, err = os.ReadFile(stderr.Name())
			require.NoError(t, err)
			wantStderr := "build stderr\n"
			if test.failure != "build" {
				wantStderr += "push stderr\n"
			}
			require.Equal(t, wantStderr, string(raw), "push diagnostics must remain separate from JSON stdout")
			raw, err = os.ReadFile(filepath.Join(dir, "fsb-args"))
			require.NoError(t, err)
			args := strings.Split(strings.TrimSpace(string(raw)), "\n")
			value := func(flag string) string {
				for i, arg := range args {
					if arg == flag && i+1 < len(args) {
						return args[i+1]
					}
				}
				t.Fatalf("missing flag %s in %q", flag, args)
				return ""
			}
			sum, err := amd64.Digest()
			require.NoError(t, err)
			require.Equal(t, tag.Context().Digest(sum.String()).Name(), value("--source"))
			require.NotContains(t, args, "--plain-http")
			require.Equal(t, filepath.Join(dir, ".dockerconfigjson"), value("--auth-file"))
			require.Equal(t, test.version, value("--kernel-version"))
			platform, err := os.ReadFile(value("--manifest-extra-file"))
			require.NoError(t, err)
			var manifest map[string]any
			require.NoError(t, json.Unmarshal(platform, &manifest))
			require.NotContains(t, manifest, "machine")
			require.NotContains(t, manifest, "compatibility")
			require.Equal(t, sum.String(), manifest["lineage"].(map[string]any)["imageDigest"])
			require.Equal(t, "512", value("--memory-mb"))
			require.Equal(t, bakedGuestIP, manifest["guestNetwork"].(map[string]any)["ip"])
			require.Equal(t, "11m0s", value("--ready-timeout"))
			require.Equal(t, buildTap, value("--tap-name"))
			require.Equal(t, "1234", value("--guest-control-port"), "the freeze/thaw probe runs on every OverlayBD build")
			require.Zero(t, sourceLayerReads.Load())
			require.NoDirExists(t, filepath.Join(dir, "oci-layout"))
			raw, err = os.ReadFile(filepath.Join(value("--inject-dir"), "etc/sandbox-init.env"))
			require.NoError(t, err)
			require.Contains(t, string(raw), "export SOURCE_ARCH='amd64'")
			require.Contains(t, string(raw), "export OVERRIDE='from-spec'")
			for _, asset := range []string{execdAssetName, bootstrapScriptName, prepareScriptName, "bwrap"} {
				raw, err = os.ReadFile(filepath.Join(value("--inject-dir"), "opt/opensandbox", asset))
				require.NoError(t, err)
				require.Equal(t, "test-runtime", string(raw))
			}
			raw, err = os.ReadFile(filepath.Join(dir, "ip.log"))
			require.NoError(t, err)
			commands := strings.Split(strings.TrimSpace(string(raw)), "\n")
			require.Equal(t, "link set dev "+buildTap+" address "+guestnetwork.GatewayMAC, commands[1])
			require.Equal(t, "link set dev "+buildTap+" up", commands[2])
			require.Equal(t, "link del "+buildTap, commands[3])
			if test.failure == "build" {
				require.NoFileExists(t, filepath.Join(dir, "push-args"))
				return
			}
			raw, err = os.ReadFile(filepath.Join(dir, "push-args"))
			require.NoError(t, err)
			args = strings.Split(strings.TrimSpace(string(raw)), "\n")
			require.NotContains(t, args, "--metadata-file", "platform metadata is packed during build, not annotated during push")
			require.Equal(t, host+"/template:"+buildID, value("--destination"))
			require.Equal(t, filepath.Join(dir, ".dockerconfigjson"), value("--auth-file"))
		})
	}
}

func captureOCIChildOutput(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	dir := t.TempDir()
	stdout, err := os.Create(filepath.Join(dir, "stdout"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stdout.Close()) })
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stderr.Close()) })
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	t.Cleanup(func() { os.Stdout, os.Stderr = oldStdout, oldStderr })
	return stdout, stderr
}

func captureOCIBuildLogs(t *testing.T) func() string {
	t.Helper()
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "logging.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("directory: %q\nalsoStderr: false\n", dir)), 0600))
	t.Setenv(componentlog.ConfigEnv, configPath)
	closeLogs, err := componentlog.Configure("sandboxtemplate-builder")
	require.NoError(t, err)
	t.Cleanup(closeLogs)
	return func() string {
		t.Helper()
		klog.Flush()
		raw, err := os.ReadFile(filepath.Join(dir, "sandboxtemplate-builder.log"))
		require.NoError(t, err)
		return string(raw)
	}
}

func requireOCIBuildPhases(t *testing.T, logs, failedPhase string) {
	t.Helper()
	var want, got []string
	for _, phase := range []string{"validate-output", "prepare-credentials", "prepare-source", "inject-runtime", "prepare-build", "build", "push", "patch-annotations"} {
		want = append(want, phase+" started")
		if phase == failedPhase {
			want = append(want, phase+" failed")
			break
		}
		want = append(want, phase+" completed")
	}
	fields := regexp.MustCompile(`phase="([^"]+)" status="([^"]+)" elapsedMs=(\d+) durationMs=(\d+)`)
	var previousElapsed int64
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"OCI template build stage"`) {
			continue
		}
		require.Regexp(t, `^[IE]\d{4} \d{2}:\d{2}:\d{2}\.\d{6} `, line, "component logging must retain timestamps")
		match := fields.FindStringSubmatch(line)
		require.Len(t, match, 5, line)
		got = append(got, match[1]+" "+match[2])
		elapsed, err := strconv.ParseInt(match[3], 10, 64)
		require.NoError(t, err)
		duration, err := strconv.ParseInt(match[4], 10, 64)
		require.NoError(t, err)
		require.GreaterOrEqual(t, elapsed, previousElapsed)
		require.LessOrEqual(t, duration, elapsed)
		previousElapsed = elapsed
		if match[2] == "started" {
			require.Zero(t, duration)
		}
		if match[2] == "failed" {
			require.True(t, strings.HasPrefix(line, "E"), line)
			require.Contains(t, line, "err=")
		} else {
			require.True(t, strings.HasPrefix(line, "I"), line)
			require.NotContains(t, line, "err=")
		}
	}
	require.Equal(t, want, got, "stages must stop at the first failure")
}

func ociTestImage(t *testing.T, architecture string) v1.Image {
	t.Helper()
	image, err := random.Image(1024, 1)
	require.NoError(t, err)
	config, err := image.ConfigFile()
	require.NoError(t, err)
	config.OS, config.Architecture = "linux", architecture
	config.Config.Env = []string{"SOURCE_ARCH=" + architecture, "OVERRIDE=from-image"}
	image, err = mutate.ConfigFile(image, config)
	require.NoError(t, err)
	return image
}

func ociTestExecd(t *testing.T) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, file := range []string{execdAssetName, bootstrapScriptName, prepareScriptName, "usr/local/bin/bwrap"} {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: file, Mode: 0755, Size: int64(len("test-runtime"))}))
		_, err := writer.Write([]byte("test-runtime"))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	layer, err := tarball.LayerFromReader(&buf)
	require.NoError(t, err)
	image, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	return image
}

func TestOCIBuilderRejectsLocalTarSource(t *testing.T) {
	t.Setenv("SANDBOX_TEMPLATE_IMAGE_TAR", "/unused/source.tar")
	spec := apiv1alpha2.SandboxTemplateSpec{Output: apiv1alpha2.OutputSpec{Format: apiv1alpha2.ArtifactFormatOverlayBD}}
	readLogs := captureOCIBuildLogs(t)
	require.ErrorContains(t, buildOCITemplate(context.Background(), spec, t.TempDir(), ""), "supported only for object-store output")
	requireOCIBuildPhases(t, readLogs(), "validate-output")
}

func TestObjectStoreSourceStillStagesLocalTar(t *testing.T) {
	image := ociTestImage(t, "amd64")
	tag, err := name.NewTag("unavailable.invalid/base:build")
	require.NoError(t, err)
	dir := t.TempDir()
	archive := filepath.Join(dir, "source.tar")
	require.NoError(t, tarball.WriteToFile(archive, tag, image))
	t.Setenv("SANDBOX_TEMPLATE_IMAGE_TAR", archive)
	sum, err := pullOCILayout(context.Background(), apiv1alpha2.SandboxTemplateSpec{Image: tag.String()}, dir)
	require.NoError(t, err)
	want, err := image.Digest()
	require.NoError(t, err)
	require.Equal(t, want.String(), sum)
	require.FileExists(t, filepath.Join(dir, "oci-layout/index.json"))
	raw, err := os.ReadFile(filepath.Join(dir, imageEnvFileName))
	require.NoError(t, err)
	require.Contains(t, string(raw), "SOURCE_ARCH=amd64")
}

func TestOCIBuildDestinationUsesSystemUUID(t *testing.T) {
	repository := "oci://registry.example.com:5000/team/templates"
	id := uuid.NewString()
	destination, err := ociBuildDestination(repository, id)
	require.NoError(t, err)
	require.Equal(t, "registry.example.com:5000/team/templates:"+id, destination)
	repeated, err := ociBuildDestination(repository, id)
	require.NoError(t, err)
	require.Equal(t, destination, repeated, "one build UUID must survive retries")
	first, err := ociBuildDestination(repository, "")
	require.NoError(t, err)
	second, err := ociBuildDestination(repository, "")
	require.NoError(t, err)
	require.NotEqual(t, first, second, "separate local builds get separate pair IDs")
	for _, target := range []string{first, second} {
		generated, err := uuid.Parse(strings.TrimPrefix(target, "registry.example.com:5000/team/templates:"))
		require.NoError(t, err)
		require.Equal(t, uuid.Version(4), generated.Version())
	}
	_, err = ociBuildDestination(repository, "not-a-uuid")
	require.ErrorContains(t, err, "invalid OCI build UUID")
}

func TestOCIBuilderRejectsTaggedOrInvalidPublishBeforeBuild(t *testing.T) {
	for _, publish := range []string{"oci://r.example/templates:tag", "oci://r.example/templates@sha256:" + strings.Repeat("a", 64), "oci://r.example/", "oci://r.example/templates:"} {
		t.Run(publish, func(t *testing.T) {
			dir := t.TempDir()
			spec := apiv1alpha2.SandboxTemplateSpec{Output: apiv1alpha2.OutputSpec{Format: apiv1alpha2.ArtifactFormatOverlayBD, Publish: publish}}
			readLogs := captureOCIBuildLogs(t)
			require.ErrorContains(t, buildOCITemplate(context.Background(), spec, dir, ""), "without a tag or digest")
			requireOCIBuildPhases(t, readLogs(), "validate-output")
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries, "invalid output must fail before credentials, source pull or build setup")
		})
	}
}
