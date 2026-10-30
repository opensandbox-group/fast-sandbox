package ocitemplate

import (
	"testing"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestRootfsUpperValidateBaseLayers(t *testing.T) {
	lower := oci.Descriptor{MediaType: OverlayBDLayerMediaType, Digest: digest.FromString("layer"), Size: 10}
	valid := RootfsUpper{Repository: "r.example/templates/base", VirtualSizeBytes: 1 << 30, Lowers: []oci.Descriptor{lower, lower}}
	require.NoError(t, valid.Validate())
	legacy := valid
	require.NoError(t, legacy.Validate(), "zero means unknown base boundary")

	negative := valid
	negative.BaseLayers = -1
	require.Error(t, negative.Validate())

	beyond := valid
	beyond.BaseLayers = 3
	require.Error(t, beyond.Validate())

	exact := valid
	exact.BaseLayers = 2
	require.NoError(t, exact.Validate())
}
