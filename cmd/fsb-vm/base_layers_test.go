package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/ocitemplate"
)

func TestManifestBaseLayers(t *testing.T) {
	key := ocitemplate.AnnotationRootfsBaseLayers
	require.Equal(t, 0, manifestBaseLayers(nil))
	require.Equal(t, 0, manifestBaseLayers(map[string]string{}))
	require.Equal(t, 7, manifestBaseLayers(map[string]string{key: "7"}))
	require.Equal(t, 0, manifestBaseLayers(map[string]string{key: "not-a-number"}))
	require.Equal(t, 0, manifestBaseLayers(map[string]string{key: "-3"}))
}
