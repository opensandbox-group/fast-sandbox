// Package hostready turns the firecracker-runtime DaemonSet into the node
// readiness authority (the kata-deploy pattern: an install/verify DaemonSet
// that labels the node once the runtime works, letting consumers select on
// the label): it checks the host for the Firecracker launch requirements
// (KVM, TUN, kernel, memory, StateRoot filesystem), installs the pinned
// Firecracker assets the runtime plan expects, materializes the StateRoot
// directory layout, and then manages the scheduling labels and the
// FirecrackerReady Node condition. A degraded environment (KVM unbound after
// a reboot, StateRoot full) is caught by the periodic recheck and the labels
// are removed again, so fastlet pods never schedule onto a broken node.
//
// The same checks ship as the standalone scripts/firecracker-host-check.sh
// for operators verifying a machine before installing anything.
package hostready

import (
	"fmt"
	"strings"
)

// Status is the outcome of one check item.
type Status string

const (
	// StatusPass means the requirement holds.
	StatusPass Status = "pass"
	// StatusWarn means the requirement holds but with a caveat (a
	// performance or robustness concern, not a launch blocker).
	StatusWarn Status = "warn"
	// StatusFail means the requirement does not hold: the node must not
	// be labeled ready.
	StatusFail Status = "fail"
)

// Check is one named verification with its outcome and a human-readable
// detail line.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is the full host-readiness outcome of one pass.
type Report struct {
	// Ready is true when no check has StatusFail.
	Ready bool `json:"ready"`
	// Summary is the one-line human-readable rollup (the Node condition
	// message).
	Summary string `json:"summary"`
	// Checks holds every item in execution order.
	Checks []Check `json:"checks"`
}

// String renders the report as a multi-line log blob.
func (r Report) String() string {
	lines := make([]string, 0, len(r.Checks)+1)
	lines = append(lines, "host ready="+fmt.Sprintf("%v", r.Ready)+" ("+r.Summary+")")
	for _, check := range r.Checks {
		lines = append(lines, fmt.Sprintf("  %-4s %-22s %s", check.Status, check.Name, check.Detail))
	}
	return strings.Join(lines, "\n")
}

// fail appends a failed check (convenience for the check runners).
func (r *Report) fail(name, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: StatusFail, Detail: detail})
}

// warn appends a warning check.
func (r *Report) warn(name, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: StatusWarn, Detail: detail})
}

// pass appends a passed check.
func (r *Report) pass(name, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: StatusPass, Detail: detail})
}

// summarize recomputes Ready and Summary from the recorded checks.
func (r *Report) summarize() {
	passes, warns, fails := 0, 0, 0
	for _, check := range r.Checks {
		switch check.Status {
		case StatusPass:
			passes++
		case StatusWarn:
			warns++
		case StatusFail:
			fails++
		}
	}
	r.Ready = fails == 0
	r.Summary = fmt.Sprintf("%d checks: %d pass, %d warn, %d fail", len(r.Checks), passes, warns, fails)
}
