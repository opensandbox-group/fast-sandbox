package guestcontrol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProtocolFencesEpochAndOperationIdentity(t *testing.T) {
	m, f := fixture(t)
	s, err := NewService(m)
	if err != nil {
		t.Fatal(err)
	}
	caps := s.Handle(Request{Protocol: Protocol, Method: "capabilities"})
	r := Request{Protocol: Protocol, Method: "freeze", Epoch: "old", Generation: 1, OperationID: "first", LeaseMillis: 1000}
	if s.Handle(r).Error == "" {
		t.Fatal("old epoch accepted")
	}
	select {
	case <-f.started:
		t.Fatal("invalid request reached filesystem")
	default:
	}
	r.Epoch = caps.Epoch
	if out := s.Handle(r); out.Error != "" || out.State != Pending {
		t.Fatalf("%+v", out)
	}
	r.Method = "thaw"
	r.LeaseMillis = 0
	r.OperationID = "different"
	if s.Handle(r).Error == "" {
		t.Fatal("operation ID mismatch accepted")
	}
	r.OperationID = "first"
	if out := s.Handle(r); out.Error != "" || out.State != Released || !out.CleanupPending {
		t.Fatalf("%+v", out)
	}
	close(f.complete)
	waitStatus(t, m, func(s Status) bool { return !s.CleanupPending && s.State == Released })
}
func TestProtocolThawBeforeFreezeAndUnknownStatus(t *testing.T) {
	m, f := fixture(t)
	s, _ := NewService(m)
	r := Request{Protocol: Protocol, Epoch: s.epoch, Generation: 2, OperationID: "second", Method: "status"}
	if s.Handle(r).Error == "" {
		t.Fatal("unknown operation reported as successful")
	}
	r.Method = "thaw"
	if s.Handle(r).Error != "" {
		t.Fatal("cannot cancel unseen request")
	}
	r.Method = "freeze"
	r.LeaseMillis = 1000
	if s.Handle(r).Error == "" {
		t.Fatal("canceled operation accepted")
	}
	r.Generation = 1
	r.OperationID = "first"
	if s.Handle(r).Error == "" {
		t.Fatal("stale generation accepted")
	}
	select {
	case <-f.started:
		t.Fatal("canceled request reached filesystem")
	default:
	}
}
func TestProtocolRejectsMalformedFramesWithoutSideEffects(t *testing.T) {
	for _, frame := range []string{
		strings.Repeat("x", MaxRequestBytes+1) + "\n",
		`{"protocol":"fsb-freeze-v1","method":"freeze","unexpected":true}` + "\n",
		`{"protocol":"fsb-freeze-v1","method":"capabilities"} {}` + "\n",
		`{"protocol":"fsb-freeze-v1"}`,
	} {
		t.Run(frame[:min(len(frame), 32)], func(t *testing.T) {
			m, f := fixture(t)
			s, _ := NewService(m)
			var out bytes.Buffer
			if s.ServeOne(strings.NewReader(frame), &out) == nil {
				t.Fatal("malformed frame accepted")
			}
			select {
			case <-f.started:
				t.Fatal("malformed request mutated filesystem")
			default:
			}
		})
	}
}
func TestProtocolCapabilitiesRoundTrip(t *testing.T) {
	m, _ := fixture(t)
	s, _ := NewService(m)
	var out bytes.Buffer
	if err := s.ServeOne(strings.NewReader(`{"protocol":"fsb-freeze-v1","method":"capabilities"}`+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var r Response
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.HelperVersion, "sha256:") || len(r.HelperVersion) != 71 || r.Protocol != Protocol || r.Epoch == "" || r.MaxLeaseMillis != time.Minute.Milliseconds() {
		t.Fatalf("%+v", r)
	}
}

func TestCapabilitiesExposeCurrentGeneration(t *testing.T) {
	manager, err := NewManager(&protocolIdentityFS{}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(manager)
	if err != nil {
		t.Fatal(err)
	}
	first := service.Handle(Request{Protocol: Protocol, Method: "capabilities"})
	if first.ProtectedMount != "/" || first.Filesystem != "ext4" {
		t.Fatalf("missing live mount identity: %+v", first)
	}
	terminal := service.Handle(Request{Protocol: Protocol, Method: "thaw", Epoch: first.Epoch, Generation: 7, OperationID: "cancel-before-freeze"})
	if terminal.Error != "" {
		t.Fatal(terminal.Error)
	}
	next := service.Handle(Request{Protocol: Protocol, Method: "capabilities"})
	if next.Generation != 7 {
		t.Fatalf("generation=%d, want 7", next.Generation)
	}
}

type protocolIdentityFS struct{}

func (*protocolIdentityFS) Freeze() error                        { return nil }
func (*protocolIdentityFS) Thaw() error                          { return nil }
func (*protocolIdentityFS) ProtectionIdentity() (string, string) { return "/", "ext4" }
