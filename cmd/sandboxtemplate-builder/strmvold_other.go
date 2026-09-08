//go:build !linux

package main

// strmvold_other.go stubs the OCI publishing stage on non-Linux platforms:
// the streamingvolume service depends on the Linux ublk/overlaybd stack and
// only ever runs inside the Linux builder image.

import (
	"context"
	"errors"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func stagePublishOCIImages(_ context.Context, _ apiv1alpha2.SandboxTemplateSpec, _, _, _, _ string, _ bool) (ociImageRefs, error) {
	return ociImageRefs{}, errors.New("OCI image publishing requires the Linux builder image (ublk/overlaybd stack)")
}
