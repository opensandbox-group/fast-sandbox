package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func TestGuestControlInjectionGates(t *testing.T) {
	t.Setenv("FSB_GUEST_CONTROL_ENABLED", "")
	enabled, err := injectGuestControl(apiv1alpha2.SandboxTemplateSpec{}, t.TempDir())
	require.NoError(t, err)
	require.False(t, enabled)
	t.Setenv("FSB_GUEST_CONTROL_ENABLED", "typo")
	_, err = injectGuestControl(apiv1alpha2.SandboxTemplateSpec{}, t.TempDir())
	require.Error(t, err)
	t.Setenv("FSB_GUEST_CONTROL_ENABLED", "1")
	_, err = injectGuestControl(apiv1alpha2.SandboxTemplateSpec{}, t.TempDir())
	require.ErrorContains(t, err, "OCI")
	spec := apiv1alpha2.SandboxTemplateSpec{}
	spec.Output.Format = apiv1alpha2.ArtifactFormatOverlayBD
	t.Setenv("FSB_GUEST_CONTROL_HELPER", filepath.Join(t.TempDir(), "missing"))
	_, err = injectGuestControl(spec, t.TempDir())
	require.Error(t, err)
}
func TestGuestControlInitOrder(t *testing.T) {
	script := renderGuestInitWithControl(apiv1alpha2.SandboxTemplateSpec{}, true)
	require.Less(t, strings.Index(script, "mount -t tmpfs tmpfs /run"), strings.Index(script, "setsid /usr/local/sbin/fsb-guest-control"))
	require.Less(t, strings.Index(script, "setsid /usr/local/sbin/fsb-guest-control"), strings.Index(script, "BOOTSTRAP_CMD="))
	require.NotContains(t, renderGuestInit(apiv1alpha2.SandboxTemplateSpec{}), "fsb-guest-control")
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// On Linux, validate injection using the actual delivery binary; this neither declares template support nor starts the service.
func TestGuestControlBinaryInjectionIntegration(t *testing.T) {
	source := os.Getenv("FSB_TEST_HELPER_BINARY")
	if source == "" {
		t.Skip("requires static Linux helper")
	}
	t.Setenv("FSB_GUEST_CONTROL_ENABLED", "1")
	t.Setenv("FSB_GUEST_CONTROL_HELPER", source)
	root := t.TempDir()
	spec := apiv1alpha2.SandboxTemplateSpec{}
	spec.Output.Format = apiv1alpha2.ArtifactFormatOverlayBD
	enabled, err := injectGuestControl(spec, root)
	require.NoError(t, err)
	require.True(t, enabled)
	want, err := os.ReadFile(source)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(root, "usr/local/sbin/fsb-guest-control"))
	require.NoError(t, err)
	require.Equal(t, want, got)
	// Image symlinks must not let injection write outside rootfs.
	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "usr/local"), 0755))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "usr/local/sbin")))
	_, err = injectGuestControl(spec, root)
	require.Error(t, err)
}

func renderGuestInit(spec apiv1alpha2.SandboxTemplateSpec) string {
	return renderGuestInitWithControl(spec, false)
}
