package artifacts

import (
	"errors"
	"testing"
)

// TestCompatibilityCPUTemplate: the node-side tier resolution behind the
// scheduling label — allowlist membership maps to the template, everything
// else (Turin, an unknown version) lands on "none".
func TestCompatibilityCPUTemplate(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		identity CPUIdentity
		want     string
	}{
		{name: "cascade lake", version: "1.16.1", identity: CPUIdentity{Vendor: VendorGenuineIntel, Family: 6, Model: 85, Stepping: 7}, want: "T2"},
		{name: "ice lake", version: "1.16.1", identity: CPUIdentity{Vendor: VendorGenuineIntel, Family: 6, Model: 106, Stepping: 6}, want: "T2"},
		{name: "milan", version: "1.16.1", identity: CPUIdentity{Vendor: VendorAuthenticAMD, Family: 25, Model: 1, Stepping: 1}, want: "T2A"},
		{name: "turin", version: "1.16.1", identity: CPUIdentity{Vendor: VendorAuthenticAMD, Family: 26, Model: 17}, want: "none"},
		{name: "skylake wrong stepping", version: "1.16.1", identity: CPUIdentity{Vendor: VendorGenuineIntel, Family: 6, Model: 85, Stepping: 5}, want: "none"},
		{name: "unknown version", version: "9.9.9", identity: CPUIdentity{Vendor: VendorGenuineIntel, Family: 6, Model: 85, Stepping: 7}, want: "none"},
	}
	for _, test := range tests {
		if got := CompatibilityCPUTemplate(test.version, test.identity); got != test.want {
			t.Fatalf("%s: CompatibilityCPUTemplate = %q, want %q", test.name, got, test.want)
		}
	}
}

// TestParseFirecrackerVersion: both historical output shapes parse — the
// attached "v1.16.1" token (current releases) and the bare "v" token
// followed by the version (older releases).
func TestParseFirecrackerVersion(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "attached v-token", output: "Firecracker v1.16.1\n", want: "1.16.1"},
		{name: "bare v-token", output: "Firecracker v 1.16.1\n", want: "1.16.1"},
		{name: "no version", output: "Firecracker\n", want: unknownProvenanceValue},
		{name: "empty", output: "", want: unknownProvenanceValue},
	}
	for _, test := range tests {
		if got := parseFirecrackerVersion(test.output); got != test.want {
			t.Fatalf("%s: parseFirecrackerVersion(%q) = %q, want %q", test.name, test.output, got, test.want)
		}
	}
}

// TestParseCPUIdentityIntel: a standard /proc/cpuinfo processor block yields
// the full structured identity, stepping included.
func TestParseCPUIdentityIntel(t *testing.T) {
	payload := []byte("processor\t: 0\n" +
		"vendor_id\t: GenuineIntel\n" +
		"cpu family\t: 6\n" +
		"model\t\t: 85\n" +
		"model name\t: Intel(R) Xeon(R) Platinum 8269CY CPU @ 2.60GHz\n" +
		"stepping\t: 7\n")
	identity := parseCPUIdentity(payload)
	if identity.Vendor != "GenuineIntel" || identity.Family != 6 || identity.Model != 85 || identity.Stepping != 7 {
		t.Fatalf("identity = %+v, want GenuineIntel family 6 model 85 stepping 7", identity)
	}
	if identity.ModelName != "Intel(R) Xeon(R) Platinum 8269CY CPU @ 2.60GHz" {
		t.Fatalf("model name = %q", identity.ModelName)
	}
}

// TestMatchRestoreCompatibility covers the tiered admission contract: the
// template allowlist tier, the unmasked identity-equality tier, legacy
// manifests, and the Firecracker version gate.
func TestMatchRestoreCompatibility(t *testing.T) {
	intelCL := CPUIdentity{Vendor: VendorGenuineIntel, Family: 6, Model: 85, Stepping: 7}
	amdTurin := CPUIdentity{Vendor: VendorAuthenticAMD, Family: 26, Model: 17}

	t2CLSnapshot := SnapshotCompatibility{
		Vendor: VendorGenuineIntel, CPUFamily: 6, CPUModel: 85, CPUStepping: 7,
		CPUTemplate: "T2", FirecrackerVersion: "1.16.1",
	}

	tests := []struct {
		name    string
		compat  SnapshotCompatibility
		local   CPUIdentity
		fcLocal string
		wantErr error
	}{
		{
			name:    "T2 snapshot restores on another allowlisted model (identity tier replaced)",
			compat:  t2CLSnapshot,
			local:   CPUIdentity{Vendor: VendorGenuineIntel, Family: 6, Model: 106, Stepping: 6},
			fcLocal: "1.16.1",
		},
		{
			name:    "T2 snapshot restores on a same-model different-stepping host",
			compat:  t2CLSnapshot,
			local:   CPUIdentity{Vendor: VendorGenuineIntel, Family: 6, Model: 85, Stepping: 4},
			fcLocal: "1.16.1",
		},
		{
			name:    "T2 snapshot rejected on a host outside the allowlist",
			compat:  t2CLSnapshot,
			local:   amdTurin,
			fcLocal: "1.16.1",
			wantErr: ErrCPUIncompatible,
		},
		{
			name:    "T2 snapshot rejected on same vendor/family/model but unlisted stepping",
			compat:  t2CLSnapshot,
			local:   CPUIdentity{Vendor: VendorGenuineIntel, Family: 6, Model: 85, Stepping: 5},
			fcLocal: "1.16.1",
			wantErr: ErrCPUIncompatible,
		},
		{
			name:    "T2A snapshot restores on Milan",
			compat:  SnapshotCompatibility{Vendor: VendorAuthenticAMD, CPUTemplate: "T2A", FirecrackerVersion: "1.16.1"},
			local:   CPUIdentity{Vendor: VendorAuthenticAMD, Family: 25, Model: 1, Stepping: 1},
			fcLocal: "1.16.1",
		},
		{
			name:    "T2A snapshot rejected on Turin",
			compat:  SnapshotCompatibility{Vendor: VendorAuthenticAMD, CPUFamily: 26, CPUModel: 17, CPUTemplate: "T2A", FirecrackerVersion: "1.16.1"},
			local:   amdTurin,
			fcLocal: "1.16.1",
			wantErr: ErrCPUIncompatible,
		},
		{
			name:    "unmasked snapshot restores on the identical identity",
			compat:  SnapshotCompatibility{Vendor: VendorAuthenticAMD, CPUFamily: 26, CPUModel: 17, CPUStepping: 0, CPUTemplate: "none", FirecrackerVersion: "1.16.1"},
			local:   amdTurin,
			fcLocal: "1.16.1",
		},
		{
			name:    "unmasked snapshot ignores stepping (Phase 1 identity)",
			compat:  SnapshotCompatibility{Vendor: VendorAuthenticAMD, CPUFamily: 26, CPUModel: 17, CPUStepping: 0, CPUTemplate: "none", FirecrackerVersion: "1.16.1"},
			local:   CPUIdentity{Vendor: VendorAuthenticAMD, Family: 26, Model: 17, Stepping: 2},
			fcLocal: "1.16.1",
		},
		{
			name:    "unmasked snapshot rejected on a different model",
			compat:  SnapshotCompatibility{Vendor: VendorGenuineIntel, CPUFamily: 6, CPUModel: 85, CPUTemplate: "none", FirecrackerVersion: "1.16.1"},
			local:   amdTurin,
			fcLocal: "1.16.1",
			wantErr: ErrCPUIncompatible,
		},
		{
			name:    "firecracker version gate fires before the CPU tier",
			compat:  t2CLSnapshot,
			local:   intelCL,
			fcLocal: "1.17.0",
			wantErr: ErrFirecrackerVersionMismatch,
		},
		{
			name:    "unknown template row for an unmatched version",
			compat:  SnapshotCompatibility{Vendor: VendorGenuineIntel, CPUTemplate: "T2", FirecrackerVersion: "9.9.9"},
			local:   intelCL,
			fcLocal: "9.9.9",
			wantErr: ErrUnknownCPUTemplate,
		},
		{
			name:    "legacy manifest reports the legacy sentinel",
			compat:  SnapshotCompatibility{},
			local:   intelCL,
			fcLocal: "1.16.1",
			wantErr: ErrLegacyCompatibility,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := MatchRestoreCompatibility(test.compat, test.local, test.fcLocal)
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("MatchRestoreCompatibility() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("MatchRestoreCompatibility() = %v, want %v", err, test.wantErr)
			}
		})
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
