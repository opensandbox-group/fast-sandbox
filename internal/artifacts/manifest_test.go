package artifacts

import (
	"testing"
)

// TestParseCPUIdentityIntel: a standard /proc/cpuinfo processor block yields
// the full structured identity.
func TestParseCPUIdentityIntel(t *testing.T) {
	payload := []byte("processor\t: 0\n" +
		"vendor_id\t: GenuineIntel\n" +
		"cpu family\t: 6\n" +
		"model\t\t: 85\n" +
		"model name\t: Intel(R) Xeon(R) Platinum 8269CY CPU @ 2.60GHz\n" +
		"stepping\t: 7\n")
	identity := parseCPUIdentity(payload)
	if identity.Vendor != "GenuineIntel" || identity.Family != 6 || identity.Model != 85 {
		t.Fatalf("identity = %+v, want GenuineIntel family 6 model 85", identity)
	}
	if identity.ModelName != "Intel(R) Xeon(R) Platinum 8269CY CPU @ 2.60GHz" {
		t.Fatalf("model name = %q", identity.ModelName)
	}
}

// TestParseCPUIdentitySharedAcrossModelNames: 8163 and 8269CY carry
// different marketing strings but the same CPUID identity (family 6, model
// 85) — the identity fields, not the name, are what consumers match.
func TestParseCPUIdentitySharedAcrossModelNames(t *testing.T) {
	skylake := parseCPUIdentity([]byte("vendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 85\nmodel name\t: Intel(R) Xeon(R) Platinum 8163 CPU @ 2.50GHz\n"))
	cascadelake := parseCPUIdentity([]byte("vendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 85\nmodel name\t: Intel(R) Xeon(R) Platinum 8269CY CPU @ 2.50GHz\n"))
	if skylake.Vendor != cascadelake.Vendor || skylake.Family != cascadelake.Family || skylake.Model != cascadelake.Model {
		t.Fatalf("identities diverge: %+v vs %+v", skylake, cascadelake)
	}
	if skylake.ModelName == cascadelake.ModelName {
		t.Fatalf("marketing strings should differ: %q", skylake.ModelName)
	}
}

// TestParseCPUIdentityAMD: a Genoa host parses with the AMD vendor identity.
func TestParseCPUIdentityAMD(t *testing.T) {
	payload := []byte("processor\t: 0\n" +
		"vendor_id\t: AuthenticAMD\n" +
		"cpu family\t: 25\n" +
		"model\t\t: 17\n" +
		"model name\t: AMD EPYC 9T24 96-Core Processor\n")
	identity := parseCPUIdentity(payload)
	if identity.Vendor != "AuthenticAMD" || identity.Family != 25 || identity.Model != 17 {
		t.Fatalf("identity = %+v, want AuthenticAMD family 25 model 17", identity)
	}
}

// TestParseCPUIdentityUnparsable: missing fields keep the unknown
// provenance placeholders instead of failing.
func TestParseCPUIdentityUnparsable(t *testing.T) {
	identity := parseCPUIdentity([]byte("not a cpuinfo payload\n"))
	if identity.Vendor != unknownProvenanceValue || identity.Family != 0 || identity.Model != 0 || identity.ModelName != "" {
		t.Fatalf("identity = %+v, want unknown vendor and zeroed identity", identity)
	}
	// The first processor block wins over later ones.
	duplicated := parseCPUIdentity([]byte("vendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 85\nmodel name\t: first\nvendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 17\nmodel name\t: second\n"))
	if duplicated.Vendor != "GenuineIntel" || duplicated.Family != 6 || duplicated.Model != 85 || duplicated.ModelName != "first" {
		t.Fatalf("identity = %+v, want the first block only", duplicated)
	}
}
