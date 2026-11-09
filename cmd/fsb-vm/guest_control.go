package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fast-sandbox/internal/guestcontrol"
)

// probeRootfsHelper verifies the actual root filesystem only in a disposable build VM; a failed probe fails the build without declaring support.
func probeRootfsHelper(ctx context.Context, socket string, port uint32) (version string, resultErr error) {
	client := guestcontrol.Client{SocketPath: socket, Port: port, Timeout: time.Second}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var caps guestcontrol.Response
	for {
		var err error
		caps, err = client.Call(probe, guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
		if err == nil {
			break
		}
		select {
		case <-probe.Done():
			return "", fmt.Errorf("guest helper handshake: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if caps.HelperVersion == "" || caps.Protocol != guestcontrol.Protocol || caps.Epoch == "" || caps.ProtectedMount != "/" || caps.Filesystem != "ext4" || caps.MaxLeaseMillis < 5000 {
		return "", errors.New("helper must protect the actual ext4 root with a compatible lease")
	}
	req := guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch, Generation: 1, OperationID: "builder-rootfs-probe", LeaseMillis: 5000}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		response, err := client.ThawAndWait(cleanup, req)
		if err == nil && (response.Epoch != req.Epoch || response.Generation != req.Generation || response.CleanupPending || (response.State != guestcontrol.Released && response.State != guestcontrol.Expired)) {
			err = errors.New("builder helper cleanup not confirmed")
		}
		resultErr = errors.Join(resultErr, err)
	}()
	response, err := client.FreezeAndWait(probe, req)
	if err != nil {
		return "", err
	}
	if response.Epoch != req.Epoch || response.Generation != req.Generation || response.State != guestcontrol.Held || response.CleanupPending {
		return "", errors.New("builder root freeze not confirmed")
	}
	return caps.HelperVersion, nil
}
