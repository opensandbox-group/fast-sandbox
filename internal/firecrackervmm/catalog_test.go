package firecrackervmm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogValidation(t *testing.T) {
	_, err := ParseCatalog(Releases)
	require.NoError(t, err)
	for _, raw := range []string{
		`{"schemaVersion":2,"legacyVersion":"1.16.1","versions":["1.16.1"]}`,
		`{"schemaVersion":1,"legacyVersion":"1.16.1","versions":["1.16.2"]}`,
		`{"schemaVersion":1,"legacyVersion":"1.16.1","versions":["1.16.1","1.16.1"]}`,
		`{"schemaVersion":1,"legacyVersion":"../x","versions":["../x"]}`,
		`{"schemaVersion":1,"legacyVersion":"01.16.1","versions":["01.16.1"]}`,
		`{"schemaVersion":1,"legacyVersion":"1.16.1","versions":["1.16.1"]} {}`,
	} {
		_, err := ParseCatalog([]byte(raw))
		require.Error(t, err, raw)
	}
}
