package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/fastletauth"
)

// TestValidateFastletAuthSubjects pins the startup contract: configurations
// that would deterministically reject every later Fastlet call must surface
// at boot, not as Forbidden on every control operation at runtime.
func TestValidateFastletAuthSubjects(t *testing.T) {
	identity := fastletauth.PodServiceAccountIdentity{Namespace: "fast-sandbox-system", ServiceAccount: "fast-sandbox-fastpath"}

	// The platform default contains the platform identities.
	require.NoError(t, validateFastletAuthSubjects(fastletauth.DefaultAllowedSubjects, identity))

	// An allow list that excludes this process's own minting identity.
	err := validateFastletAuthSubjects([]string{"system:serviceaccount:fast-sandbox-system:fast-sandbox-controller"}, identity)
	require.ErrorContains(t, err, "every Fastlet call would fail with Forbidden")

	// A subject no ServiceAccount token can ever present (missing the
	// system:serviceaccount: prefix).
	err = validateFastletAuthSubjects([]string{"fast-sandbox-fastpath"}, identity)
	require.ErrorContains(t, err, "no ServiceAccount token can present it")

	// A subject with a malformed namespace:name shape.
	err = validateFastletAuthSubjects([]string{"system:serviceaccount:only-a-namespace"}, identity)
	require.ErrorContains(t, err, "not a parseable service account identity")

	// A custom-namespace deployment relying on the built-in default is
	// caught at startup instead of failing closed at the first call.
	custom := fastletauth.PodServiceAccountIdentity{Namespace: "auth-ns", ServiceAccount: "controller"}
	err = validateFastletAuthSubjects(fastletauth.DefaultAllowedSubjects, custom)
	require.ErrorContains(t, err, "system:serviceaccount:auth-ns:controller")
}
