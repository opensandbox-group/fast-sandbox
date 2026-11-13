package fastletauth

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBearerToken(t *testing.T) {
	header := http.Header{}
	_, found := BearerToken(header)
	require.False(t, found, "an absent Authorization header must not authenticate")

	header.Set("Authorization", "Basic dXNlcjpwYXNz")
	_, found = BearerToken(header)
	require.False(t, found, "a non-bearer scheme must not authenticate")

	header.Set("Authorization", BearerPrefix)
	_, found = BearerToken(header)
	require.False(t, found, "an empty credential must not authenticate")

	header.Set("Authorization", BearerPrefix+"token-value")
	token, found := BearerToken(header)
	require.True(t, found)
	require.Equal(t, "token-value", token)
}

func TestSubjectFromDefaultsToEmpty(t *testing.T) {
	require.Empty(t, SubjectFrom(t.Context()))
}
