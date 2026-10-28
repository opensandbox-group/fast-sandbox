package vmmetadata

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoundTrip(t *testing.T) {
	directory := t.TempDir()
	payload := `{"version":1,"uid":"6f9c1e8a-3b2d-4c5e-8f70-a1b2c3d4e5f6","namespace":"team-a","name":"sbx-7d9f2c","placementAttempt":2}`
	require.NoError(t, os.WriteFile(filepath.Join(directory, FileName), []byte(payload), 0o640))

	record, err := Read(directory)
	require.NoError(t, err)
	require.Equal(t, Metadata{
		Version: 1, UID: "6f9c1e8a-3b2d-4c5e-8f70-a1b2c3d4e5f6",
		Namespace: "team-a", Name: "sbx-7d9f2c", PlacementAttempt: 2,
	}, record)
}

func TestReadMissingIsRetryable(t *testing.T) {
	_, err := Read(t.TempDir())
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestParseRejectsShortPrefixAndInvalidRecords(t *testing.T) {
	for _, payload := range []string{
		`{"version":1,"uid":"6f9c1e8a"`,      // short prefix of a single-write
		`{"version":0,"uid":"u"}`,            // invalid version
		`{"version":1,"namespace":"team-a"}`, // missing uid
		`not json at all`,
	} {
		_, err := Parse([]byte(payload))
		require.Error(t, err, "payload %q", payload)
	}
}

func TestParseToleratesUnknownFutureFields(t *testing.T) {
	record, err := Parse([]byte(`{"version":1,"uid":"u","namespace":"ns","name":"n","futureField":true}`))
	require.NoError(t, err)
	require.Equal(t, Metadata{Version: 1, UID: "u", Namespace: "ns", Name: "n"}, record)
}
