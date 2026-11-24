package firecracker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCaptureJournalPersistsBeforeEffects(t *testing.T) {
	f := &captureFixture{}
	tx := f.transaction()
	dir := filepath.Join(t.TempDir(), "capture")
	j, err := newCaptureJournal(dir, captureRecord{Incarnation: "boot-1", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: tx.Request})
	require.NoError(t, err)
	tx.Progress = j
	tx.Flush = func() error {
		recovered, e := loadCaptureJournal(dir)
		require.NoError(t, e)
		require.Equal(t, captureFlushing, recovered.record.Phase)
		return nil
	}
	tx.Cut = func() error {
		recovered, e := loadCaptureJournal(dir)
		require.NoError(t, e)
		require.Equal(t, captureCutting, recovered.record.Phase)
		return nil
	}
	require.NoError(t, tx.run(t.Context()))
	recovered, err := loadCaptureJournal(dir)
	require.NoError(t, err)
	require.Equal(t, captureRecovered, recovered.record.Phase)
	_, err = newCaptureJournal(dir, j.record)
	require.Error(t, err)
	require.Error(t, j.Record(captureFreezing))
}

func TestCaptureJournalRejectsLostPersistence(t *testing.T) {
	f := &captureFixture{}
	tx := f.transaction()
	dir := filepath.Join(t.TempDir(), "capture")
	j, err := newCaptureJournal(dir, captureRecord{Incarnation: "boot-1", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: tx.Request})
	require.NoError(t, err)
	tx.Progress = j
	require.NoError(t, os.RemoveAll(dir))
	require.Error(t, tx.run(t.Context()))
	require.Equal(t, []string{"verify"}, f.calls)
}

func TestCaptureJournalQuarantineSurvivesReload(t *testing.T) {
	f := &captureFixture{}
	tx := f.transaction()
	dir := filepath.Join(t.TempDir(), "capture")
	j, err := newCaptureJournal(dir, captureRecord{Incarnation: "boot-1", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: tx.Request})
	require.NoError(t, err)
	require.NoError(t, j.Record(captureFreezing))
	require.NoError(t, j.Quarantine(errors.New("guest epoch changed")))
	recovered, err := loadCaptureJournal(dir)
	require.NoError(t, err)
	require.Equal(t, captureQuarantined, recovered.record.Phase)
	require.Contains(t, recovered.record.Failure, "epoch")
	require.Error(t, recovered.Record(captureRecovering))
}

func TestCaptureTransactionPersistsQuarantine(t *testing.T) {
	for _, failure := range []string{"resume", "thaw", "release"} {
		t.Run(failure, func(t *testing.T) {
			f := &captureFixture{fail: failure}
			tx := f.transaction()
			dir := filepath.Join(t.TempDir(), "capture")
			j, err := newCaptureJournal(dir, captureRecord{Incarnation: "boot-1", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: tx.Request})
			require.NoError(t, err)
			tx.Progress = j
			require.Error(t, tx.run(t.Context()))
			saved, err := loadCaptureJournal(dir)
			require.NoError(t, err)
			require.Equal(t, captureQuarantined, saved.record.Phase)
		})
	}
}

func TestCaptureJournalInitialFailureDoesNotFence(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "rootfs-capture")
	temporary := filepath.Join(root, ".capture-init-failed")
	require.NoError(t, os.Mkdir(temporary, 0700))
	// Force the initial rename to fail after the temporary record was written.
	require.NoError(t, os.Mkdir(filepath.Join(temporary, "capture.json"), 0700))
	fixture := &captureFixture{}
	record := captureRecord{Incarnation: "boot", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: fixture.transaction().Request}
	_, err := publishInitialCaptureJournal(directory, temporary, record)
	require.Error(t, err)
	require.NoDirExists(t, directory)
	require.NoDirExists(t, temporary)
	// An interrupted initialization has no visible fence; a new attempt succeeds.
	orphan := filepath.Join(root, ".capture-init-interrupted")
	require.NoError(t, os.Mkdir(orphan, 0700))
	journal, err := newCaptureJournal(directory, record)
	require.NoError(t, err)
	loaded, err := loadCaptureJournal(directory)
	require.NoError(t, err)
	require.Equal(t, journal.record, loaded.record)
}

func TestCaptureJournalPreservesExistingEmptyFence(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "rootfs-capture")
	require.NoError(t, os.Mkdir(directory, 0700))
	fixture := &captureFixture{}
	_, err := newCaptureJournal(directory, captureRecord{Incarnation: "boot", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: fixture.transaction().Request})
	require.ErrorIs(t, err, os.ErrExist)
	require.DirExists(t, directory)
}

func TestNativeCaptureJournalVersionAndIdentity(t *testing.T) {
	tx := (&captureFixture{}).transaction()
	r := captureRecord{Backend: "native", FileDevice: 42, FileInode: 123, Incarnation: "vm", Device: "/disk.img", Staging: "/cut", Request: tx.Request}
	path := filepath.Join(t.TempDir(), "journal")
	j, err := newCaptureJournal(path, r)
	require.NoError(t, err)
	require.Equal(t, 2, j.record.Version, "old readers must reject native records")
	reloaded, err := loadCaptureJournal(path)
	require.NoError(t, err)
	require.Equal(t, uint64(123), reloaded.record.FileInode)
	for _, mutate := range []func(*captureRecord){func(r *captureRecord) { r.Version = 1 }, func(r *captureRecord) { r.FileInode = 0 }, func(r *captureRecord) { r.Backend = "unknown" }} {
		bad := j.record
		mutate(&bad)
		payload, err := json.Marshal(bad)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(path, "capture.json"), payload, 0600))
		_, err = loadCaptureJournal(path)
		require.Error(t, err)
	}
}
