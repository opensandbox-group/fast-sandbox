package guestcontrol

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
)

const Protocol = "fsb-freeze-v1"
const MaxRequestBytes = 4096

// Request is a single-line JSON record on a stream authenticated as the host.
// Capabilities discovers the agent epoch; other operations must echo that epoch.
type Request struct {
	Protocol    string `json:"protocol"`
	Method      string `json:"method"`
	Epoch       string `json:"epoch,omitempty"`
	Generation  uint64 `json:"generation,omitempty"`
	OperationID string `json:"operationId,omitempty"`
	LeaseMillis int64  `json:"leaseMillis,omitempty"`
}
type Response struct {
	HelperVersion  string `json:"helperVersion,omitempty"`
	ProtectedMount string `json:"protectedMount,omitempty"`
	Filesystem     string `json:"filesystem,omitempty"`
	Protocol       string `json:"protocol"`
	Epoch          string `json:"epoch"`
	MaxLeaseMillis int64  `json:"maxLeaseMillis"`
	Generation     uint64 `json:"generation,omitempty"`
	State          State  `json:"state,omitempty"`
	CleanupPending bool   `json:"cleanupPending"`
	Error          string `json:"error,omitempty"`
	Failure        string `json:"failure,omitempty"`
}

// Service binds each generation to a unique operation ID.
// Callers provide transport authentication, source incarnation fencing, and restart reconciliation.
type Service struct {
	helperVersion string
	mu            sync.Mutex
	manager       *Manager
	epoch         string
	generation    uint64
	operationID   string
}

func NewService(manager *Manager) (*Service, error) {
	if manager == nil {
		return nil, errors.New("manager required")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	version, err := helperExecutableVersion()
	if err != nil {
		return nil, err
	}
	return &Service{manager: manager, epoch: hex.EncodeToString(nonce[:]), helperVersion: version}, nil
}

var helperExecutableVersion = sync.OnceValues(func() (string, error) {
	// Fingerprint the running executable, independently of the wire protocol.
	// Linux exposes the loaded inode even if a deployment replaced its path.
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		if runtime.GOOS == "linux" {
			return "", err
		}
		path, pathErr := os.Executable()
		if pathErr != nil {
			return "", pathErr
		}
		file, err = os.Open(path)
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
})

func (s *Service) Handle(r Request) Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Response{HelperVersion: s.helperVersion, Protocol: Protocol, Epoch: s.epoch, Generation: s.generation, MaxLeaseMillis: s.manager.maxLease.Milliseconds()}
	if identity, ok := s.manager.fs.(interface{ ProtectionIdentity() (string, string) }); ok {
		out.ProtectedMount, out.Filesystem = identity.ProtectionIdentity()
	}
	fail := func(message string) Response { out.Error = message; return out }
	if r.Protocol != Protocol {
		return fail("unsupported protocol")
	}
	if r.Method == methodCapabilities {
		return out
	}
	if r.Epoch != s.epoch {
		return fail("agent epoch mismatch")
	}
	if r.Method != methodFreeze && r.Method != methodThaw && r.Method != methodStatus {
		return fail("unknown method")
	}
	if r.Generation == 0 || r.OperationID == "" || len(r.OperationID) > 128 {
		return fail("generation and operation ID required")
	}
	if r.Generation < s.generation {
		return fail("stale generation")
	}
	if r.Generation == s.generation && r.OperationID != s.operationID {
		return fail("operation ID mismatch")
	}
	if r.Method == methodFreeze && (r.LeaseMillis <= 0 || r.LeaseMillis > s.manager.maxLease.Milliseconds()) {
		return fail("invalid lease")
	}
	if r.Method != methodFreeze && r.LeaseMillis != 0 {
		return fail("lease only allowed on freeze")
	}
	var state Status
	var err error
	switch r.Method {
	case methodFreeze:
		state, err = s.manager.Freeze(r.Generation, time.Duration(r.LeaseMillis)*time.Millisecond)
	case methodThaw:
		state, err = s.manager.Thaw(r.Generation)
	case methodStatus:
		state, err = s.manager.Status(r.Generation)
	}
	if err != nil {
		return fail(err.Error())
	}
	if r.Generation > s.generation {
		s.generation = r.Generation
		s.operationID = r.OperationID
	}
	out.Generation = state.Generation
	out.State = state.State
	out.CleanupPending = state.CleanupPending
	if state.Failure != nil {
		out.Failure = state.Failure.Error()
	}
	return out
}

// ServeOne rejects malformed or oversized frames before executing any mutation.
// The transport must enforce deadlines and limit concurrent connections.
func (s *Service) ServeOne(reader io.Reader, writer io.Writer) error {
	// Read byte by byte to avoid waiting for EOF on an active connection.
	// Requests are intentionally small, with one operation per connection.
	frame := make([]byte, 0, 256)
	var b [1]byte
	for len(frame) <= MaxRequestBytes {
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			return err
		}
		if b[0] == '\n' {
			break
		}
		frame = append(frame, b[0])
	}
	if len(frame) > MaxRequestBytes {
		return errors.New("request too large")
	}
	var req Request
	// Reject unknown fields so protocol version mismatches fail safely.
	dec := json.NewDecoder(bytes.NewReader(frame))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON request")
	}
	return json.NewEncoder(writer).Encode(s.Handle(req))
}

const (
	methodCapabilities = "capabilities"
	methodFreeze       = "freeze"
	methodThaw         = "thaw"
	methodStatus       = "status"
)
