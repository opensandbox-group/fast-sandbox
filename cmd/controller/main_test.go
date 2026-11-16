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

	// The effective default derives from the control plane's own namespace,
	// so a same-convention deployment outside fast-sandbox-system is valid.
	customSubjects := fastletauth.PlatformSubjectsForNamespace("auth-ns")
	require.NoError(t, validateFastletAuthSubjects(customSubjects,
		fastletauth.PodServiceAccountIdentity{Namespace: "auth-ns", ServiceAccount: "fast-sandbox-controller"}))

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

	// A derived default still fails when the deployment's own ServiceAccount
	// does not follow the platform naming convention.
	err = validateFastletAuthSubjects(customSubjects,
		fastletauth.PodServiceAccountIdentity{Namespace: "auth-ns", ServiceAccount: "controller"})
	require.ErrorContains(t, err, "system:serviceaccount:auth-ns:controller")
}
