package main

import (
	"context"

	"fast-sandbox/internal/guestcontrol"
)

func probeRootfsHelper(ctx context.Context, socket string, port uint32) (string, error) {
	return guestcontrol.ProbeRootFilesystem(ctx, socket, port)
}
