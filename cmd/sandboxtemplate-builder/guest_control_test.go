package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// staticHelperBinary writes a minimal statically linked ELF64 image for the
// current architecture: enough structure to pass injectGuestControl's
// validation (machine match, no PT_INTERP), not a real executable. The raw
// fixed-width mirrors avoid encoding/binary's rejection of debug/elf's
// stringer-typed header structs.
func staticHelperBinary(t *testing.T) string {
	t.Helper()
	machine := uint16(elf.EM_X86_64)
	if runtime.GOARCH == "arm64" {
		machine = uint16(elf.EM_AARCH64)
	}
	type elfHeaderRaw struct {
		Ident     [16]byte
		Type      uint16
		Machine   uint16
		Version   uint32
		Entry     uint64
		Phoff     uint64
		Shoff     uint64
		Flags     uint32
		Ehsize    uint16
		Phentsize uint16
		Phnum     uint16
		Shentsize uint16
		Shnum     uint16
		Shstrndx  uint16
	}
	type progHeaderRaw struct {
		Type   uint32
		Flags  uint32
		Off    uint64
		Vaddr  uint64
		Paddr  uint64
		Filesz uint64
		Memsz  uint64
		Align  uint64
	}
	var buf bytes.Buffer
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, elfHeaderRaw{
		Ident:     [16]byte{0x7f, 'E', 'L', 'F', 2, 1, 1},
		Type:      uint16(elf.ET_EXEC),
		Machine:   machine,
		Version:   uint32(elf.EV_CURRENT),
		Phoff:     64,
		Ehsize:    64,
		Phentsize: 56,
		Phnum:     1,
	}))
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, progHeaderRaw{Type: uint32(elf.PT_LOAD), Flags: uint32(elf.PF_R | elf.PF_X)}))
	path := filepath.Join(t.TempDir(), "fsb-guest-control")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o755))
	return path
}

func TestGuestControlInjection(t *testing.T) {
	spec := apiv1alpha2.SandboxTemplateSpec{}
	spec.Output.Format = apiv1alpha2.ArtifactFormatOverlayBD
	t.Setenv("FSB_GUEST_CONTROL_HELPER", filepath.Join(t.TempDir(), "missing"))
	// The retired FSB_GUEST_CONTROL_ENABLED gate has no effect in either
	// direction: a missing helper fails OverlayBD builds with or without it.
	t.Setenv("FSB_GUEST_CONTROL_ENABLED", "0")
	_, err := injectGuestControl(spec, t.TempDir())
	require.Error(t, err)
	t.Setenv("FSB_GUEST_CONTROL_ENABLED", "1")
	_, err = injectGuestControl(spec, t.TempDir())
	require.Error(t, err)
	// Native (object-store) builds never inject and never fail on the helper.
	enabled, err := injectGuestControl(apiv1alpha2.SandboxTemplateSpec{}, t.TempDir())
	require.NoError(t, err)
	require.False(t, enabled)
	// OverlayBD injection requires a static helper matching the builder arch.
	t.Setenv("FSB_GUEST_CONTROL_HELPER", staticHelperBinary(t))
	root := t.TempDir()
	enabled, err = injectGuestControl(spec, root)
	require.NoError(t, err)
	require.True(t, enabled)
	_, err = os.Stat(filepath.Join(root, "usr/local/sbin/fsb-guest-control"))
	require.NoError(t, err)
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
