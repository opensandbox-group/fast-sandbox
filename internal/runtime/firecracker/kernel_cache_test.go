package firecracker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/firecrackervmm"
)

func TestPinBootKernelCachesByFileIdentityAndIsolatesMutation(t *testing.T) {
	stateRoot := t.TempDir()
	source := filepath.Join(t.TempDir(), "vmlinux-6.1")
	require.NoError(t, os.WriteFile(source, []byte("kernel-v1"), 0o600))

	d := &Driver{}
	target1 := filepath.Join(stateRoot, "boot-1")
	sum1, err := d.pinBootKernel(stateRoot, source, target1)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("kernel-v1"))), sum1)

	target2 := filepath.Join(stateRoot, "boot-2")
	sum2, err := d.pinBootKernel(stateRoot, source, target2)
	require.NoError(t, err)
	require.Equal(t, sum1, sum2)

	info1, err := os.Stat(target1)
	require.NoError(t, err)
	info2, err := os.Stat(target2)
	require.NoError(t, err)
	require.True(t, os.SameFile(info1, info2), "warm hits must hard-link the cached immutable inode")

	// Existing target must not be overwritten.
	_, err = d.pinBootKernel(stateRoot, source, target1)
	require.ErrorIs(t, err, os.ErrExist)

	// In-place source mutation must not alter previously pinned targets, and the
	// next boot must detect the updated file identity and re-pin.
	require.NoError(t, os.WriteFile(source, []byte("kernel-v2-updated"), 0o600))
	raw1, err := os.ReadFile(target1)
	require.NoError(t, err)
	require.Equal(t, "kernel-v1", string(raw1))

	target3 := filepath.Join(stateRoot, "boot-3")
	sum3, err := d.pinBootKernel(stateRoot, source, target3)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("kernel-v2-updated"))), sum3)
	raw3, err := os.ReadFile(target3)
	require.NoError(t, err)
	require.Equal(t, "kernel-v2-updated", string(raw3))
}

func TestBootCompatibilityReusesSelectedVMMVersionWithoutExec(t *testing.T) {
	d := &Driver{}
	selected := &firecrackervmm.Selection{
		Version:    "1.17.0",
		BinaryPath: "/nonexistent/must-not-exec/firecracker",
	}
	compat := d.bootCompatibility(selected)
	require.Equal(t, artifacts.HostCPUArch(), compat.CPUArch)
	require.Equal(t, artifacts.CPUTemplateNone, compat.CPUTemplate)
	require.Equal(t, "1.17.0", compat.FirecrackerVersion)
	require.Equal(t, artifacts.HostKernelRelease(), compat.HostKernel)
}

func TestWaitForAPISocketDetectsFastSocketWithinBackoff(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "api.sock")
	go func() {
		time.Sleep(3 * time.Millisecond)
		_ = os.WriteFile(socketPath, nil, 0o600)
	}()
	started := time.Now()
	require.NoError(t, waitForAPISocket(context.Background(), socketPath, time.Second))
	require.Less(t, time.Since(started), 18*time.Millisecond, "initial 2ms backoff should detect socket faster than old 20ms poll")
}
