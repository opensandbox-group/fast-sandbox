package firecracker

import (
	"fmt"
	"strconv"
	"strings"

	"fast-sandbox/internal/vmmexec"
)

// FSEP-0003 launch-time assertion. After every Firecracker create the VMM
// process must show the fixed capability set absent from its effective and
// bounding sets, with PR_SET_NO_NEW_PRIVS applied. The assertion is
// unconditional (both launch modes, no configuration): the capability state
// is process state, never persisted, so this is the only durable proof that
// the drop took effect.

// vmmCapabilityStatus is the parsed capability and no-new-privileges state
// of one process from /proc/<pid>/status.
type vmmCapabilityStatus struct {
	inheritable uint64
	permitted   uint64
	effective   uint64
	bounding    uint64
	noNewPrivs  bool
}

// parseVMMCapabilityStatus parses the CapInh/CapPrm/CapEff/CapBnd and
// NoNewPrivs fields of a /proc/<pid>/status document.
func parseVMMCapabilityStatus(content string) (vmmCapabilityStatus, error) {
	var status vmmCapabilityStatus
	fields := map[string]*uint64{
		"CapInh": &status.inheritable,
		"CapPrm": &status.permitted,
		"CapEff": &status.effective,
		"CapBnd": &status.bounding,
	}
	sawNoNewPrivs := false
	for line := range strings.SplitSeq(content, "\n") {
		name, value, found := strings.Cut(line, ":\t")
		if !found {
			continue
		}
		if target, ok := fields[strings.TrimSpace(name)]; ok {
			parsed, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
			if err != nil {
				return status, fmt.Errorf("parse %s value %q: %w", name, value, err)
			}
			*target = parsed
			continue
		}
		if strings.TrimSpace(name) == "NoNewPrivs" {
			sawNoNewPrivs = true
			status.noNewPrivs = strings.TrimSpace(value) == "1"
		}
	}
	if !sawNoNewPrivs {
		return status, fmt.Errorf("status has no NoNewPrivs field")
	}
	return status, nil
}

// verifyCapabilitiesDropped requires the fixed drop set to be absent from
// the effective and bounding sets and NoNewPrivs to be set. The assertion
// proves the bits are absent; operation-level denial of BPF/perf while
// CAP_SYS_ADMIN is retained is established by the seccomp policy check
// (seccomp_policy_test.go) and becomes cap-complete in a later batch that
// removes sys_admin.
func (s vmmCapabilityStatus) verifyCapabilitiesDropped() error {
	mask := vmmexec.DropMask()
	for _, set := range []struct {
		field string
		value uint64
	}{{"CapEff", s.effective}, {"CapBnd", s.bounding}} {
		if overlap := set.value & mask; overlap != 0 {
			capability, _ := vmmexec.Offender(overlap)
			return fmt.Errorf("%s still holds the dropped capability %q (mask %#x)", set.field, capability.Name, overlap)
		}
	}
	if !s.noNewPrivs {
		return fmt.Errorf("NoNewPrivs is not set on the VMM process")
	}
	return nil
}
