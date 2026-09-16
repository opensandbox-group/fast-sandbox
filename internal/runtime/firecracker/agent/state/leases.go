// Package state implements the durable lease and reference-count state of
// the firecracker-runtime (implementation plan §5):
//
//   - per-Sandbox device leases (native stage: cache file paths);
//   - per-image reference counts (pin count + active lease count);
//   - an append-only JSON journal that orders every mutating RPC — the
//     intent line is durable before the side effect runs, the result line
//     after it commits (two-phase commit) — so replays return the first
//     execution's outcome exactly once and crash recovery rebuilds the
//     lease table and the idempotency cache.
//
// Every completed mutating RPC is bound to the caller PodUID: a replay of
// the same request id, or a release of a foreign lease, by a different pod
// fails with ErrConflict.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"

	"k8s.io/klog/v2"
)

// Op identifies a journaled mutating RPC.
type Op string

const (
	OpPinImage       Op = "pin-image"
	OpUnpinImage     Op = "unpin-image"
	OpLeaseDevices   Op = "lease-devices"
	OpReleaseDevices Op = "release-devices"
)

// JournalFileName is the append-only journal under the StateRoot.
const JournalFileName = "journal.log"

// Sentinel errors the server maps onto protocol error codes.
var (
	// ErrConflict reports an idempotency key or ownership mismatch: the
	// request id was already committed by a different pod, or the op
	// collides with state owned by another pod.
	ErrConflict = errors.New("conflict")
)

// Options configures the state for tests.
type Options struct {
	Now  func() time.Time
	UUID func() (string, error)
}

// Option is a functional state option.
type Option func(*Options)

// WithNow overrides the clock.
func WithNow(now func() time.Time) Option {
	return func(options *Options) { options.Now = now }
}

// WithUUID overrides the lease id generator.
func WithUUID(uuid func() (string, error)) Option {
	return func(options *Options) { options.UUID = uuid }
}

// imageRefs tracks how many independent references keep an image cached.
type imageRefs struct {
	pinCount   int
	leaseCount int
}

// completed is the cached outcome of a committed mutating RPC.
type completed struct {
	op        Op
	podUID    string
	pinDigest string
	lease     *agentprotocol.Lease
}

// inflight deduplicates concurrent calls with the same request id: the
// first caller runs the side effect, the rest wait for its outcome.
type inflight struct {
	done   chan struct{}
	result any
	err    error
}

// State owns the lease table, the image reference counts, and the journal.
type State struct {
	mu         sync.Mutex
	now        func() time.Time
	uuid       func() (string, error)
	journal    *journal
	leases     map[string]agentprotocol.Lease
	bySandbox  map[string]string
	sandboxKey map[string]*sync.Mutex
	images     map[string]*imageRefs
	completed  map[string]completed
	inflight   map[string]*inflight
}

// New loads (or creates) the journal under stateRoot and rebuilds the lease
// table, reference counts, and idempotency cache from its completed entries.
func New(stateRoot string, options ...Option) (*State, error) {
	config := Options{}
	for _, option := range options {
		option(&config)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.UUID == nil {
		config.UUID = newUUID
	}
	journalDir := filepath.Join(stateRoot, "agent")
	journal, err := openJournal(filepath.Join(journalDir, JournalFileName))
	if err != nil {
		return nil, err
	}
	state := &State{
		now: config.Now, uuid: config.UUID, journal: journal,
		leases: make(map[string]agentprotocol.Lease), bySandbox: make(map[string]string),
		sandboxKey: make(map[string]*sync.Mutex), images: make(map[string]*imageRefs),
		completed: make(map[string]completed), inflight: make(map[string]*inflight),
	}
	if err := state.recover(); err != nil {
		_ = journal.close()
		return nil, err
	}
	return state, nil
}

// Close flushes and closes the journal.
func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.close()
}

// HealthSnapshot is the read-only view of the agent state.
type HealthSnapshot struct {
	Leases     []agentprotocol.Lease
	LeaseCount int
	PinCount   int
	ImageCount int
}

// Snapshot returns the current leases and reference-count totals.
func (s *State) Snapshot() HealthSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := HealthSnapshot{
		Leases:     make([]agentprotocol.Lease, 0, len(s.leases)),
		LeaseCount: len(s.leases),
		ImageCount: len(s.images),
	}
	for _, lease := range s.leases {
		snapshot.Leases = append(snapshot.Leases, lease)
	}
	for _, refs := range s.images {
		snapshot.PinCount += refs.pinCount
	}
	return snapshot
}

// PinImage records one pin reference for an image. Replaying a completed
// request id returns the recorded manifest digest without re-executing the
// side effect; a concurrent duplicate waits for the in-flight execution.
// The do callback runs outside the state lock (a multi-GiB pull must not
// block other RPCs) and must return the pinned manifest digest.
func (s *State) PinImage(id agentprotocol.Identity, image string, do func() (string, error)) (string, error) {
	result, err := s.run(id, OpPinImage, pinImageArgs{Image: image}, func() (any, error) {
		digest, err := do()
		if err != nil {
			return nil, err
		}
		return pinImageResult{ManifestDigest: digest}, nil
	}, func(result any) {
		if s.images[image] == nil {
			s.images[image] = &imageRefs{}
		}
		s.images[image].pinCount++
	})
	if err != nil {
		return "", err
	}
	digest, _ := result.(pinImageResult)
	return digest.ManifestDigest, nil
}

// UnpinImage drops one pin reference of an image. The count is clamped at
// zero; unpinning an unknown or unpinned image is a no-op (but still
// journaled so the request id stays idempotent).
func (s *State) UnpinImage(id agentprotocol.Identity, image string) error {
	_, err := s.run(id, OpUnpinImage, unpinImageArgs{Image: image}, func() (any, error) {
		return nil, nil
	}, func(result any) {
		if refs := s.images[image]; refs != nil {
			refs.pinCount--
			if refs.pinCount < 0 {
				refs.pinCount = 0
			}
		}
	})
	return err
}

// LeaseDevices leases the devices (native stage: cache file paths) of an
// image for one Sandbox. The same request id replays the recorded lease;
// a different request id for the same sandbox id returns the existing
// lease (business idempotency, matching the driver's EnsureSandbox). The
// do callback runs outside the state lock and receives the generated lease
// id; it must return the complete lease record (paths, manifest digest).
func (s *State) LeaseDevices(id agentprotocol.Identity, sandboxID, image string, memSizeMiB int, rootfsWritable bool, do func(leaseID string) (agentprotocol.Lease, error)) (agentprotocol.Lease, error) {
	// The sandbox business key serializes concurrent leases of the same
	// Sandbox: exactly one lease is created and every later request (any
	// request id) returns it.
	key := s.sandboxLock(sandboxID)
	key.Lock()
	defer key.Unlock()

	s.mu.Lock()
	if existing, ok := s.bySandbox[sandboxID]; ok {
		lease := s.leases[existing]
		s.mu.Unlock()
		if lease.PodUID != id.PodUID {
			return agentprotocol.Lease{}, conflictf("sandbox %q is leased by pod %s", sandboxID, lease.PodUID)
		}
		if err := s.rememberCompleted(id, OpLeaseDevices, &lease); err != nil {
			return agentprotocol.Lease{}, err
		}
		return lease, nil
	}
	s.mu.Unlock()

	result, err := s.run(id, OpLeaseDevices, leaseDevicesArgs{
		SandboxID: sandboxID, Image: image, MemSizeMiB: memSizeMiB, RootfsWritable: rootfsWritable,
	}, func() (any, error) {
		leaseID, err := s.uuid()
		if err != nil {
			return nil, err
		}
		return do(leaseID)
	}, func(result any) {
		lease := result.(agentprotocol.Lease)
		s.leases[lease.LeaseID] = lease
		s.bySandbox[lease.SandboxID] = lease.LeaseID
		if s.images[lease.Image] == nil {
			s.images[lease.Image] = &imageRefs{}
		}
		s.images[lease.Image].leaseCount++
	})
	if err != nil {
		return agentprotocol.Lease{}, err
	}
	lease, _ := result.(agentprotocol.Lease)
	return lease, nil
}

// sandboxLock returns (creating as needed) the per-Sandbox serialization
// lock.
func (s *State) sandboxLock(sandboxID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.sandboxKey[sandboxID]
	if key == nil {
		key = &sync.Mutex{}
		s.sandboxKey[sandboxID] = key
	}
	return key
}

// ReleaseDevices drops a device lease. The caller must own the lease: a
// release by a different pod fails with ErrConflict.
func (s *State) ReleaseDevices(id agentprotocol.Identity, leaseID string) error {
	s.mu.Lock()
	lease, ok := s.leases[leaseID]
	if ok && lease.PodUID != id.PodUID {
		s.mu.Unlock()
		return conflictf("lease %q is owned by pod %s", leaseID, lease.PodUID)
	}
	s.mu.Unlock()

	_, err := s.run(id, OpReleaseDevices, releaseDevicesArgs{LeaseID: leaseID}, func() (any, error) {
		return nil, nil
	}, func(result any) {
		lease, ok := s.leases[leaseID]
		if !ok {
			return
		}
		delete(s.leases, leaseID)
		delete(s.bySandbox, lease.SandboxID)
		if refs := s.images[lease.Image]; refs != nil {
			refs.leaseCount--
			if refs.leaseCount < 0 {
				refs.leaseCount = 0
			}
		}
	})
	return err
}

// GetLease returns a lease by id.
func (s *State) GetLease(leaseID string) (agentprotocol.Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.leases[leaseID]
	return lease, ok
}

// run is the generic two-phase journaled execution shared by the mutating
// RPCs: completed-request replay, in-flight dedup, intent journal, side
// effect, in-memory apply, result journal, completion record.
//
// Self-healing window: the in-memory apply runs before the result line is
// durable. If that result write fails (disk failure), the process memory is
// changed but the journal holds only the intent — after a crash the op is
// dropped, its idempotency key with it, and a replay re-executes the side
// effect. Re-execution is safe: pulls are idempotent, leases are rebuilt
// under a fresh lease id, and the in-memory counts die with the process.
// The failed caller still sees an error and can retry.
func (s *State) run(id agentprotocol.Identity, op Op, args any, do func() (any, error), apply func(result any)) (any, error) {
	s.mu.Lock()
	if entry, ok := s.completed[id.RequestID]; ok {
		s.mu.Unlock()
		if entry.op != op {
			return nil, conflictf("request id %q was already committed as %s", id.RequestID, entry.op)
		}
		if entry.podUID != id.PodUID {
			return nil, conflictf("request id %q was committed by pod %s", id.RequestID, entry.podUID)
		}
		return entry.result(), nil
	}
	if pending, ok := s.inflight[id.RequestID]; ok {
		s.mu.Unlock()
		<-pending.done
		return pending.result, pending.err
	}
	if err := s.journalAppendLocked(journalEntry{
		RequestID: id.RequestID, Op: string(op), PodUID: id.PodUID, Namespace: id.Namespace,
		Args: mustJSON(args), At: s.now(),
	}); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("journal %s intent: %w", op, err)
	}
	pending := &inflight{done: make(chan struct{})}
	s.inflight[id.RequestID] = pending
	s.mu.Unlock()

	result, err := do()
	if err != nil {
		s.finish(id.RequestID, pending, nil, err)
		return nil, err
	}

	// The apply must never leave the state lock held, even on a panic.
	s.mu.Lock()
	committed := false
	defer func() {
		if !committed {
			s.mu.Unlock()
		}
	}()
	apply(result)
	entry := completed{op: op, podUID: id.PodUID}
	entry.setResult(result)
	s.completed[id.RequestID] = entry
	resultPayload := mustJSON(result)
	if result == nil {
		resultPayload = emptyResultJSON
	}
	if err := s.journalAppendLocked(journalEntry{
		RequestID: id.RequestID, Op: string(op), Result: resultPayload, At: s.now(),
	}); err != nil {
		committed = true
		s.mu.Unlock()
		s.finish(id.RequestID, pending, nil, fmt.Errorf("journal %s result: %w", op, err))
		return nil, err
	}
	committed = true
	s.mu.Unlock()
	s.finish(id.RequestID, pending, result, nil)
	return result, nil
}

// finish removes the in-flight marker and wakes waiters.
func (s *State) finish(requestID string, pending *inflight, result any, err error) {
	s.mu.Lock()
	delete(s.inflight, requestID)
	s.mu.Unlock()
	pending.result = result
	pending.err = err
	close(pending.done)
}

// rememberCompleted records an idempotent replay of an existing lease so a
// later retry of the same request id returns the same lease without
// touching the journal.
func (s *State) rememberCompleted(id agentprotocol.Identity, op Op, lease *agentprotocol.Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.completed[id.RequestID]; ok {
		if existing.podUID != id.PodUID {
			return conflictf("request id %q was committed by pod %s", id.RequestID, existing.podUID)
		}
		return nil
	}
	s.completed[id.RequestID] = completed{op: op, podUID: id.PodUID, lease: lease}
	return nil
}

// recover replays the journal in file order: an intent line with a
// following result line is a committed op and its effect is rebuilt; an
// intent without a result is a crash mid-execution and is dropped. A
// trailing partial line (crash mid-append) truncates the log.
func (s *State) recover() error {
	intents := make(map[string]journalEntry)
	truncateLength, err := s.journal.replay(func(entry journalEntry) error {
		if len(entry.Result) == 0 {
			if entry.RequestID == "" {
				return nil
			}
			intents[entry.RequestID] = entry
			return nil
		}
		intent, ok := intents[entry.RequestID]
		delete(intents, entry.RequestID)
		if !ok {
			return nil
		}
		return s.replayApply(intent, entry)
	})
	if err != nil {
		return err
	}
	if truncateLength > 0 {
		// A crash-torn journal tail changes which intents replay; recovery
		// decisions must be auditable.
		klog.InfoS("Recovered agent lease journal; truncating torn tail", "truncateLength", truncateLength)
		if err := s.journal.truncate(truncateLength); err != nil {
			return fmt.Errorf("truncate agent journal tail: %w", err)
		}
	}
	return nil
}

// replayApply rebuilds the effect of one committed journal entry pair.
func (s *State) replayApply(intent, result journalEntry) error {
	op := Op(intent.Op)
	completed := completed{op: op, podUID: intent.PodUID}
	switch op {
	case OpPinImage:
		var args pinImageArgs
		if err := json.Unmarshal(intent.Args, &args); err != nil {
			return fmt.Errorf("journal: decode %s intent: %w", op, err)
		}
		if err := s.bumpPin(args.Image, 1); err != nil {
			return err
		}
		var response pinImageResult
		if err := json.Unmarshal(result.Result, &response); err != nil {
			return fmt.Errorf("journal: decode %s result: %w", op, err)
		}
		completed.pinDigest = response.ManifestDigest
	case OpUnpinImage:
		var args unpinImageArgs
		if err := json.Unmarshal(intent.Args, &args); err != nil {
			return fmt.Errorf("journal: decode %s intent: %w", op, err)
		}
		if err := s.bumpPin(args.Image, -1); err != nil {
			return err
		}
	case OpLeaseDevices:
		var lease agentprotocol.Lease
		if err := json.Unmarshal(result.Result, &lease); err != nil {
			return fmt.Errorf("journal: decode %s result: %w", op, err)
		}
		s.leases[lease.LeaseID] = lease
		s.bySandbox[lease.SandboxID] = lease.LeaseID
		if s.images[lease.Image] == nil {
			s.images[lease.Image] = &imageRefs{}
		}
		s.images[lease.Image].leaseCount++
		completed.lease = &lease
	case OpReleaseDevices:
		var args releaseDevicesArgs
		if err := json.Unmarshal(intent.Args, &args); err != nil {
			return fmt.Errorf("journal: decode %s intent: %w", op, err)
		}
		lease, ok := s.leases[args.LeaseID]
		if ok {
			delete(s.leases, args.LeaseID)
			delete(s.bySandbox, lease.SandboxID)
			if refs := s.images[lease.Image]; refs != nil {
				refs.leaseCount--
			}
		}
	default:
		return fmt.Errorf("journal: unknown op %q", op)
	}
	s.completed[intent.RequestID] = completed
	return nil
}

// bumpPin adjusts an image's pin count, creating the entry as needed and
// clamping at zero.
func (s *State) bumpPin(image string, delta int) error {
	if image == "" {
		return fmt.Errorf("journal: pin op without image")
	}
	refs := s.images[image]
	if refs == nil {
		refs = &imageRefs{}
		s.images[image] = refs
	}
	refs.pinCount += delta
	if refs.pinCount < 0 {
		refs.pinCount = 0
	}
	return nil
}

// journalAppendLocked writes one line and fsyncs it. The caller holds the
// state lock, so the intent/result pair stays ordered relative to every
// other state mutation.
func (s *State) journalAppendLocked(entry journalEntry) error {
	return s.journal.append(entry)
}

// completedResult returns the typed outcome of a completed op for replay.
func (c completed) result() any {
	switch c.op {
	case OpPinImage:
		return pinImageResult{ManifestDigest: c.pinDigest}
	case OpLeaseDevices:
		if c.lease != nil {
			return *c.lease
		}
		return nil
	default:
		return nil
	}
}

// setResult stores the typed outcome of a completed op.
func (c *completed) setResult(result any) {
	switch c.op {
	case OpPinImage:
		if pin, ok := result.(pinImageResult); ok {
			c.pinDigest = pin.ManifestDigest
		}
	case OpLeaseDevices:
		if lease, ok := result.(agentprotocol.Lease); ok {
			c.lease = &lease
		}
	}
}

// newUUID returns a random UUID v4 without external dependencies.
func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(raw[:])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32], nil
}

func conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

func mustJSON(value any) []byte {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return payload
}
