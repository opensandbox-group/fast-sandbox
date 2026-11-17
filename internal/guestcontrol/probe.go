package guestcontrol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// ProbeRootFilesystem verifies the actual root filesystem only in a disposable build VM; a failed probe fails the build without declaring support.
func ProbeRootFilesystem(ctx context.Context, socket string, port uint32) (version string, resultErr error) {
	client := Client{SocketPath: socket, Port: port, Timeout: time.Second}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var caps Response
	for {
		var err error
		caps, err = client.Call(probe, Request{Protocol: Protocol, Method: "capabilities"})
		if err == nil {
			break
		}
		select {
		case <-probe.Done():
			return "", fmt.Errorf("guest helper handshake: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if caps.HelperVersion == "" || caps.Protocol != Protocol || caps.Epoch == "" || caps.ProtectedMount != "/" || caps.Filesystem != "ext4" || caps.MaxLeaseMillis < 10000 || caps.Generation == math.MaxUint64 {
		return "", errors.New("helper must protect the actual ext4 root with a compatible lease")
	}
	req := Request{Protocol: Protocol, Epoch: caps.Epoch, Generation: caps.Generation + 1, OperationID: "builder-rootfs-probe", LeaseMillis: 5000}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		response, err := client.ThawAndWait(cleanup, req)
		if err == nil && (response.Epoch != req.Epoch || response.Generation != req.Generation || response.CleanupPending || (response.State != Released && response.State != Expired)) {
			err = errors.New("builder helper cleanup not confirmed")
		}
		resultErr = errors.Join(resultErr, err)
	}()
	response, err := client.FreezeAndWait(probe, req)
	if err != nil {
		return "", err
	}
	if response.Epoch != req.Epoch || response.Generation != req.Generation || response.State != Held || response.CleanupPending {
		return "", errors.New("builder root freeze not confirmed")
	}
	return caps.HelperVersion, nil
}
