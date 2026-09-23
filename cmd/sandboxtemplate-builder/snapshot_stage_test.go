package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWaitForAnyMarkerReturnsTheMatchedMarker: the boot gate distinguishes
// guest readiness from the init's startup-failure exit path.
func TestWaitForAnyMarkerReturnsTheMatchedMarker(t *testing.T) {
	tests := []struct {
		name    string
		log     string
		want    string
		wantErr string
	}{
		{
			name: "readiness marker matches",
			log:  "kernel boot noise\nSANDBOX_READY\n",
			want: "SANDBOX_READY",
		},
		{
			name: "startup failure marker matches",
			log:  "kernel boot noise\nSANDBOX_STARTUP_FAILED no_loopback_tool\n",
			want: startupFailedMarker,
		},
		{
			name:    "no marker times out with the log tail",
			log:     "kernel boot noise only\n",
			wantErr: "timed out waiting",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "boot.console.log")
			if err := os.WriteFile(logPath, []byte(test.log), 0o644); err != nil {
				t.Fatalf("stage log: %v", err)
			}
			got, err := waitForAnyMarker(logPath, 200*time.Millisecond, "SANDBOX_READY", startupFailedMarker)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				if got != "" {
					t.Fatalf("marker = %q, want empty on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("waitForAnyMarker: %v", err)
			}
			if got != test.want {
				t.Fatalf("marker = %q, want %q", got, test.want)
			}
		})
	}
}

// TestWaitForAnyMarkerReadsAppendedBytes: the console log grows while the
// VM boots, so a marker written after polling started must still match.
func TestWaitForAnyMarkerReadsAppendedBytes(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "boot.console.log")
	if err := os.WriteFile(logPath, []byte("booting\n"), 0o644); err != nil {
		t.Fatalf("stage log: %v", err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		_, _ = file.WriteString("SANDBOX_READY\n")
		_ = file.Close()
	}()
	got, err := waitForAnyMarker(logPath, 5*time.Second, "SANDBOX_READY")
	if err != nil {
		t.Fatalf("waitForAnyMarker: %v", err)
	}
	if got != "SANDBOX_READY" {
		t.Fatalf("marker = %q, want SANDBOX_READY", got)
	}
}

// TestLastMarkerLineReturnsTheLatestMatch: the startup-failure error names
// the most recent reason the init printed, not an earlier one.
func TestLastMarkerLineReturnsTheLatestMatch(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "boot.console.log")
	payload := "SANDBOX_STARTUP_FAILED first\nnoise\nSANDBOX_STARTUP_FAILED no_loopback_tool\n"
	if err := os.WriteFile(logPath, []byte(payload), 0o644); err != nil {
		t.Fatalf("stage log: %v", err)
	}
	if got, want := lastMarkerLine(logPath, startupFailedMarker), "SANDBOX_STARTUP_FAILED no_loopback_tool"; got != want {
		t.Fatalf("lastMarkerLine = %q, want %q", got, want)
	}
}

// TestLastMarkerLineMissingLogFallsBackToMarker: an unreadable log still
// yields the bare marker instead of an empty failure reason.
func TestLastMarkerLineMissingLogFallsBackToMarker(t *testing.T) {
	if got := lastMarkerLine(filepath.Join(t.TempDir(), "missing.log"), startupFailedMarker); got != startupFailedMarker {
		t.Fatalf("lastMarkerLine = %q, want %q", got, startupFailedMarker)
	}
}

// TestCPUTemplateForVendor: the pinned static template follows the host CPU
// vendor — T2 on Intel, T2A on AMD (the two vendor-native baselines) — and
// an unknown vendor pins none (the snapshot then carries the raw host
// CPUID; hosts whose model refuses the vendor template take the
// bootPreparationVM fallback).
func TestCPUTemplateForVendor(t *testing.T) {
	tests := []struct {
		vendor string
		want   string
	}{
		{"GenuineIntel", "T2"},
		{"AuthenticAMD", "T2A"},
		{"unknown", ""},
		{"", ""},
		{"GenuineBochs", ""},
	}
	for _, test := range tests {
		if got := cpuTemplateForVendor(test.vendor); got != test.want {
			t.Fatalf("cpuTemplateForVendor(%q) = %q, want %q", test.vendor, got, test.want)
		}
	}
}
