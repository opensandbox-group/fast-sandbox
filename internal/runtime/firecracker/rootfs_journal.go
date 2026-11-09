package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"fast-sandbox/internal/guestcontrol"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type capturePhase string

const (
	capturePrepared    capturePhase = "prepared"
	captureFreezing    capturePhase = "freezing"
	capturePausing     capturePhase = "pausing"
	captureFlushing    capturePhase = "flushing"
	captureCutting     capturePhase = "cutting"
	captureRecovering  capturePhase = "recovering"
	captureRecovered   capturePhase = "recovered"
	captureAborted     capturePhase = "aborted"
	captureQuarantined capturePhase = "quarantined"
)

// captureRecord persists intent before side effects; phase names do not imply remote completion.
// Incarnation must bind to the actual VMM startup instance, not merely a reusable PID.
type captureRecord struct {
	// PublicationOwned means the independent copy belongs to the publication flow; recovered refers only to source VM recovery.
	PublicationOwned bool                        `json:"publicationOwned,omitempty"`
	Owner            *fastletapi.SandboxIdentity `json:"owner,omitempty"`
	Version          int                         `json:"version"`
	Incarnation      string                      `json:"incarnation"`
	Device           string                      `json:"device"`
	Staging          string                      `json:"staging"`
	Request          guestcontrol.Request        `json:"request"`
	Phase            capturePhase                `json:"phase"`
	Failure          string                      `json:"failure,omitempty"`
}

type captureProgress interface {
	Record(capturePhase) error
	Quarantine(error) error
}

// captureJournal only manages durable records; callers must hold the cross-process lifecycle lock.
// Retain records until the upper layer confirms the outcome; restart or timeout must not automatically delete them and start over.
type captureJournal struct {
	mu        sync.Mutex
	directory string
	record    captureRecord
}

func newCaptureJournal(directory string, r captureRecord) (*captureJournal, error) {
	if r.Incarnation == "" || !filepath.IsAbs(r.Device) || !filepath.IsAbs(r.Staging) || r.Request.Protocol != guestcontrol.Protocol || r.Request.Epoch == "" || r.Request.Generation == 0 || r.Request.OperationID == "" {
		return nil, errors.New("incomplete capture journal identity")
	}

	// The caller holds the lifecycle lock. Never replace an existing journal,
	// including a corrupt or incomplete record from an older implementation.
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		if err == nil {
			err = os.ErrExist
		}
		return nil, fmt.Errorf("reserve capture journal: %w", err)
	}
	temporary, err := os.MkdirTemp(filepath.Dir(directory), ".capture-init-")
	if err != nil {
		return nil, err
	}
	return publishInitialCaptureJournal(directory, temporary, r)
}

// Prepare the entire initial record privately before making the fence visible.
// A crash before rename can leave only an inert temporary directory, never an
// empty rootfs-capture fence. No Guest/VM effects may start before this returns.
func publishInitialCaptureJournal(directory, temporary string, r captureRecord) (journal *captureJournal, resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temporary)) }()
	r.Version = 1
	r.Phase = capturePrepared
	j := &captureJournal{directory: temporary, record: r}
	if err := j.persist(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, directory); err != nil {
		return nil, err
	}
	j.directory = directory
	if err := syncCaptureDirectory(filepath.Dir(directory)); err != nil {
		// No side effects have started. Roll back our complete but unacknowledged
		// reservation; if cleanup fails, preserve the error and recoverable record.
		return nil, errors.Join(err, os.RemoveAll(directory), syncCaptureDirectory(filepath.Dir(directory)))
	}
	return j, nil
}

func loadCaptureJournal(directory string) (*captureJournal, error) {
	raw, err := os.ReadFile(filepath.Join(directory, "capture.json"))
	if err != nil {
		return nil, err
	}
	var r captureRecord
	if err = json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Version != 1 || r.Incarnation == "" || !filepath.IsAbs(r.Device) || !filepath.IsAbs(r.Staging) || r.Request.Protocol != guestcontrol.Protocol || r.Request.Epoch == "" || r.Request.Generation == 0 || r.Request.OperationID == "" {
		return nil, errors.New("invalid capture recovery record")
	}
	switch r.Phase {
	case capturePrepared, captureFreezing, capturePausing, captureFlushing, captureCutting, captureRecovering, captureRecovered, captureAborted, captureQuarantined:
	default:
		return nil, errors.New("unknown capture recovery phase")
	}
	return &captureJournal{directory: directory, record: r}, nil
}

// beginVerifiedRecovery is reserved for the reconciler after identity validation.
// Ordinary progress cannot release quarantine; the lifecycle lock serializes
// validation and this durable transition with replacement and deletion.
func (j *captureJournal) beginVerifiedRecovery() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.record.Phase == captureRecovered || j.record.Phase == captureAborted {
		return errors.New("capture already terminal")
	}
	old := j.record
	j.record.Phase = captureRecovering
	if err := j.persist(); err != nil {
		j.record = old
		return err
	}
	return nil
}

func (j *captureJournal) Record(next capturePhase) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	current := j.record.Phase
	valid := next == captureRecovering && current != captureRecovered && current != captureQuarantined && current != captureAborted ||
		current == capturePrepared && next == captureFreezing || current == captureFreezing && next == capturePausing ||
		current == capturePausing && next == captureFlushing || current == captureFlushing && next == captureCutting ||
		current == captureRecovering && next == captureRecovered ||
		next == captureAborted && current != captureRecovered && current != captureAborted
	if !valid {
		return fmt.Errorf("invalid capture transition %s -> %s", current, next)
	}
	old := j.record
	j.record.Phase = next
	if err := j.persist(); err != nil {
		j.record = old
		return err
	}
	return nil
}
func (j *captureJournal) Quarantine(cause error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.record.Phase = captureQuarantined
	if cause != nil {
		j.record.Failure = cause.Error()
	}
	return j.persist()
}
func (j *captureJournal) persist() error {
	raw, err := json.Marshal(j.record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(j.directory, ".capture-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(j.directory, "capture.json")); err != nil {
		return err
	}
	return syncCaptureDirectory(j.directory)
}
func syncCaptureDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
