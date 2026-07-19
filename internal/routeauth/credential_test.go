package routeauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCredentialRoundTripAndFencing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := NewIssuer(privateKey, 30*time.Second, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := NewVerifier(publicKey, func() time.Time { return now })
	require.NoError(t, err)

	expected := Claims{
		Tenant: "tenant-a", Namespace: "default", SandboxUID: "uid-a", TargetPort: 8080,
		FastletPodUID: "pod-a", AssignmentAttempt: 2, RouteGeneration: 3,
	}
	token, issued, err := issuer.Issue(expected)
	require.NoError(t, err)
	require.Equal(t, now.Add(30*time.Second).Unix(), issued.ExpiresAt)
	require.NotEmpty(t, issued.Nonce)

	actual, err := verifier.VerifyExpected(token, expected)
	require.NoError(t, err)
	require.Equal(t, issued, actual)

	stale := expected
	stale.RouteGeneration++
	_, err = verifier.VerifyExpected(token, stale)
	require.ErrorIs(t, err, ErrClaimMismatch)

	wrongPort := expected
	wrongPort.TargetPort = 8081
	_, err = verifier.VerifyExpected(token, wrongPort)
	require.ErrorIs(t, err, ErrClaimMismatch)
}

func TestCredentialRejectsTamperAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := NewIssuer(privateKey, time.Second, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := NewVerifier(publicKey, func() time.Time { return now.Add(2 * time.Second) })
	require.NoError(t, err)
	token, _, err := issuer.Issue(Claims{
		Namespace: "default", SandboxUID: "uid-a", TargetPort: 80, FastletPodUID: "pod-a",
		AssignmentAttempt: 1, RouteGeneration: 1,
	})
	require.NoError(t, err)

	_, err = verifier.Verify(token)
	require.ErrorIs(t, err, ErrExpiredCredential)

	tampered := token[:len(token)-1] + "x"
	_, err = verifier.Verify(tampered)
	require.True(t, errors.Is(err, ErrInvalidCredential) || errors.Is(err, ErrExpiredCredential))
}

func TestVerifierSetSupportsOverlappingKeyRotation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	oldPublicKey, oldPrivateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	newPublicKey, newPrivateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	oldIssuer, err := NewIssuer(oldPrivateKey, 5*time.Minute, func() time.Time { return now })
	require.NoError(t, err)
	newIssuer, err := NewIssuer(newPrivateKey, 5*time.Minute, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := NewVerifierSet([]ed25519.PublicKey{oldPublicKey, newPublicKey}, func() time.Time { return now })
	require.NoError(t, err)
	claims := Claims{
		Namespace: "default", SandboxUID: "uid-a", TargetPort: 8080, FastletPodUID: "pod-a",
		AssignmentAttempt: 1, RouteGeneration: 1,
	}

	oldToken, _, err := oldIssuer.Issue(claims)
	require.NoError(t, err)
	newToken, _, err := newIssuer.Issue(claims)
	require.NoError(t, err)
	_, err = verifier.VerifyExpected(oldToken, claims)
	require.NoError(t, err)
	_, err = verifier.VerifyExpected(newToken, claims)
	require.NoError(t, err)

	encoded := base64.StdEncoding.EncodeToString(oldPublicKey) + "," + base64.RawStdEncoding.EncodeToString(newPublicKey)
	parsed, err := ParsePublicKeySet(encoded)
	require.NoError(t, err)
	require.Equal(t, []ed25519.PublicKey{oldPublicKey, newPublicKey}, parsed)
}
