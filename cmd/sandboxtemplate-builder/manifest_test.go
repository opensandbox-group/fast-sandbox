package main

import (
	"testing"
)

// TestCompatibilityCPUTemplate: the manifest records the template the
// snapshot stage actually booted with; an empty effective template (the raw
// host CPUID fallback) is recorded as "none" instead of an empty field.
func TestCompatibilityCPUTemplate(t *testing.T) {
	if got, want := compatibilityCPUTemplate("T2"), "T2"; got != want {
		t.Fatalf("compatibilityCPUTemplate = %q, want %q", got, want)
	}
	if got, want := compatibilityCPUTemplate("T2A"), "T2A"; got != want {
		t.Fatalf("compatibilityCPUTemplate = %q, want %q", got, want)
	}
	if got, want := compatibilityCPUTemplate(""), "none"; got != want {
		t.Fatalf("compatibilityCPUTemplate = %q, want %q", got, want)
	}
}
