package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeSourceImage(t *testing.T) {
	for _, testCase := range []struct{ in, want string }{
		{"alpine", "docker.io/library/alpine"},
		{"alpine:3.19", "docker.io/library/alpine:3.19"},
		{"team/app", "docker.io/team/app"},
		{"localhost:5000/x", "localhost:5000/x"},
		{"127.0.0.1:5000/templates", "127.0.0.1:5000/templates"},
		{"ghcr.io/org/img:tag", "ghcr.io/org/img:tag"},
	} {
		require.Equal(t, testCase.want, normalizeSourceImage(testCase.in), testCase.in)
	}
}
