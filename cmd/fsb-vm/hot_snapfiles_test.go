package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedHotSnapfilesDir(t *testing.T, dir string, withOptional bool) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	files := map[string]string{
		inDiskVMState:  "vmstate",
		inDiskMemfile:  "memfile",
		inDiskManifest: `{"schemaVersion":1}`,
	}
	if withOptional {
		files["vault.json"] = `{"v":1}`
		files["policy.json"] = `{"p":1}`
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
}

func TestAttachHotSnapfilesLinksInodesAndRejectsInvalid(t *testing.T) {
	source := t.TempDir()
	seedHotSnapfilesDir(t, source, true)

	mountPoint := filepath.Join(t.TempDir(), snapfilesMount)
	require.NoError(t, attachHotSnapfiles(source, mountPoint))

	for _, name := range []string{inDiskVMState, inDiskMemfile, inDiskManifest, "vault.json", "policy.json"} {
		srcInfo, err := os.Lstat(filepath.Join(source, name))
		require.NoError(t, err)
		dstInfo, err := os.Lstat(filepath.Join(mountPoint, name))
		require.NoError(t, err)
		require.True(t, os.SameFile(srcInfo, dstInfo), "hot snapfiles %s must be hard-linked", name)
	}

	incomplete := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(incomplete, inDiskVMState), []byte("vmstate"), 0o600))
	require.Error(t, attachHotSnapfiles(incomplete, filepath.Join(t.TempDir(), snapfilesMount)))

	symlinked := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(symlinked, inDiskVMState), []byte("vmstate"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(symlinked, inDiskManifest), []byte(`{"schemaVersion":1}`), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(source, inDiskMemfile), filepath.Join(symlinked, inDiskMemfile)))
	require.ErrorContains(t, attachHotSnapfiles(symlinked, filepath.Join(t.TempDir(), snapfilesMount)), "not a non-empty regular file")
}

type fakeDeviceReleaser struct {
	devices  []ublkdDevice
	released []int
}

func (f *fakeDeviceReleaser) List(_ context.Context) ([]ublkdDevice, error) {
	return f.devices, nil
}

func (f *fakeDeviceReleaser) Release(_ context.Context, devID int) error {
	f.released = append(f.released, devID)
	return nil
}

func TestReleaseSandboxHotSnapfilesSkipsUnmountAndDropsPins(t *testing.T) {
	root := t.TempDir()
	oldStateDir, oldRuntimeDir := global.stateDir, global.runtimeDir
	global.stateDir = filepath.Join(root, "sandboxes")
	global.runtimeDir = filepath.Join(root, "runtime")
	t.Cleanup(func() {
		global.stateDir, global.runtimeDir = oldStateDir, oldRuntimeDir
	})

	hotSource := filepath.Join(root, "hot-entry")
	seedHotSnapfilesDir(t, hotSource, false)

	layout := newSandboxLayout("sb-hot")
	require.NoError(t, os.MkdirAll(layout.home, 0o750))
	require.NoError(t, os.MkdirAll(layout.runtime, 0o750))
	require.NoError(t, attachHotSnapfiles(hotSource, layout.mountPoint))

	memStat, err := os.Lstat(filepath.Join(hotSource, inDiskMemfile))
	require.NoError(t, err)
	require.EqualValues(t, 2, memStat.Sys().(*syscall.Stat_t).Nlink, "attached sandbox pins hot memfile")

	state := &sandboxState{
		SandboxID:    "sb-hot",
		Phase:        phaseRunning,
		HotSnapfiles: true,
		MountPoint:   layout.mountPoint,
		APISocket:    layout.apiSocket,
		Rootfs:       &deviceRecord{Config: layout.rootfsConfig, DevID: 7, DevPath: "/dev/ublkb7", Acquired: true},
	}
	require.NoError(t, writeState(layout.home, state))

	releaser := &fakeDeviceReleaser{
		devices: []ublkdDevice{
			{DevID: 7, Config: layout.rootfsConfig},
			{DevID: 9, Config: layout.snapfilesConfig},
		},
	}
	require.NoError(t, releaseSandbox(context.Background(), releaser, layout, state))
	require.Equal(t, []int{7}, releaser.released, "only rootfs device should be released; snapfiles device must be skipped")
	require.Nil(t, state.Rootfs)

	require.NoError(t, removeSandboxDirs(layout))
	memStat, err = os.Lstat(filepath.Join(hotSource, inDiskMemfile))
	require.NoError(t, err)
	require.EqualValues(t, 1, memStat.Sys().(*syscall.Stat_t).Nlink, "sandbox removal must drop hot cache pin")
}
