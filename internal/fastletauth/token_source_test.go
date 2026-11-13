package fastletauth

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolvePodServiceAccountIdentityPrefersEnvironment(t *testing.T) {
	identity, err := ResolvePodServiceAccountIdentity("ctrl-ns", "ctrl-sa")
	require.NoError(t, err)
	require.Equal(t, PodServiceAccountIdentity{Namespace: "ctrl-ns", ServiceAccount: "ctrl-sa"}, identity)
}

func TestResolvePodServiceAccountIdentityRejectsIncompleteIdentity(t *testing.T) {
	_, err := ResolvePodServiceAccountIdentity("ctrl-ns", "")
	require.Error(t, err)
}

func TestResolvePodIssuerPrefersEnvironment(t *testing.T) {
	issuer, err := ResolvePodIssuer("https://issuer.example")
	require.NoError(t, err)
	require.Equal(t, "https://issuer.example", issuer)
}

func TestTokenRequestSourceMintsOnceAndRefreshes(t *testing.T) {
	var mintCalls int
	source := NewTokenRequestSource(func(ctx context.Context, namespace, name string, request *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		mintCalls++
		require.Equal(t, "ctrl-ns", namespace)
		require.Equal(t, "ctrl-sa", name)
		require.Equal(t, []string{Audience}, request.Spec.Audiences)
		require.NotNil(t, request.Spec.ExpirationSeconds)
		require.Equal(t, int64(DefaultExpirationSeconds), *request.Spec.ExpirationSeconds)
		return &authenticationv1.TokenRequest{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Status: authenticationv1.TokenRequestStatus{
				Token:               "token-" + string(rune('a'+mintCalls)),
				ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(*request.Spec.ExpirationSeconds) * time.Second)),
			},
		}, nil
	}, PodServiceAccountIdentity{Namespace: "ctrl-ns", ServiceAccount: "ctrl-sa"}, Audience)

	first, err := source.Token(t.Context())
	require.NoError(t, err)
	second, err := source.Token(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, second, "the cached token must be reused before the refresh margin")
	require.Equal(t, 1, mintCalls)

	// Advance past the refresh margin (TTL 1h, margin 20min): the next call
	// mints a new token.
	later := time.Now().Add(DefaultExpirationSeconds*time.Second - DefaultRefreshBefore + 5*time.Minute)
	source.now = func() time.Time { return later }
	third, err := source.Token(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, first, third)
	require.Equal(t, 2, mintCalls)
}

func TestTokenRequestSourceClampsRefreshMarginToShortLifetimes(t *testing.T) {
	var mintCalls int
	base := time.Now()
	now := base
	source := NewTokenRequestSource(func(ctx context.Context, namespace, name string, request *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		mintCalls++
		// The cluster granted a lifetime shorter than the refresh margin (as
		// when --service-account-max-token-expiration clamps the request).
		return &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token:               "token-" + strconv.Itoa(mintCalls),
			ExpirationTimestamp: metav1.NewTime(base.Add(time.Second)),
		}}, nil
	}, PodServiceAccountIdentity{Namespace: "ns", ServiceAccount: "sa"}, Audience)
	source.RefreshBefore = time.Minute

	_, err := source.Token(t.Context())
	require.NoError(t, err)
	_, err = source.Token(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, mintCalls, "the margin must clamp to half the granted lifetime instead of re-minting per call")
	// Past the clamped horizon (lifetime/2 = 0.5s) the source re-mints.
	now = base.Add(600 * time.Millisecond)
	source.now = func() time.Time { return now }
	_, err = source.Token(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, mintCalls)
}

func TestTokenRequestSourceWrapsFailuresAsUnavailable(t *testing.T) {
	source := NewTokenRequestSource(func(ctx context.Context, namespace, name string, request *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		return nil, errors.New("forbidden: no serviceaccounts/token grant")
	}, PodServiceAccountIdentity{Namespace: "ns", ServiceAccount: "sa"}, Audience)
	_, err := source.Token(t.Context())
	require.ErrorIs(t, err, ErrUnavailable)
	require.True(t, strings.Contains(err.Error(), "forbidden"), "the underlying RBAC error must stay visible for diagnosis")
}

func TestTokenRequestSourceDefaultsAudience(t *testing.T) {
	var audiences []string
	source := NewTokenRequestSource(func(ctx context.Context, namespace, name string, request *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		audiences = request.Spec.Audiences
		return &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token:               "token",
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour)),
		}}, nil
	}, PodServiceAccountIdentity{Namespace: "ns", ServiceAccount: "sa"}, "")
	_, err := source.Token(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{Audience}, audiences)
}
