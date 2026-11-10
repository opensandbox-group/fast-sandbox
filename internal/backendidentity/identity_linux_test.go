//go:build linux

package backendidentity

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBackendQualificationRejectsStaleRecord(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("node qualification requires root")
	}
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listen := func() net.Listener {
		l, err := net.Listen("unix", socket)
		require.NoError(t, err)
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		return l
	}
	l := listen()
	defer func() { l.Close() }()
	sum, err := hashFile("/proc/self/exe")
	require.NoError(t, err)
	require.Error(t, Verify(t.Context(), socket, sum))
	require.NoError(t, Attest(t.Context(), socket, sum))
	require.NoError(t, Verify(t.Context(), socket, sum))
	require.Error(t, Verify(t.Context(), socket, "wrong"))
	path := socket + ".qualification.json"
	require.NoError(t, os.Chmod(path, 0666))
	require.Error(t, Verify(t.Context(), socket, sum))
	require.NoError(t, os.Chmod(path, 0600))
	l.Close()
	l = listen()
	require.ErrorContains(t, Verify(t.Context(), socket, sum), "stale")
	require.NoError(t, Attest(t.Context(), socket, sum))
	require.NoError(t, Verify(t.Context(), socket, sum))
	require.NoError(t, os.Rename(path, path+".saved"))
	require.NoError(t, os.Symlink(path+".saved", path))
	require.Error(t, Verify(t.Context(), socket, sum))
}

func TestBackendQualificationAcrossPIDNamespace(t *testing.T) {
	if os.Getenv("FSB_TEST_PID_NAMESPACE") != "1" {
		t.Skip("requires root and unshare on disposable Linux host")
	}
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	sum, err := hashFile("/proc/self/exe")
	require.NoError(t, err)
	require.NoError(t, Attest(t.Context(), socket, sum))
	command := exec.Command("unshare", "--mount", "--pid", "--fork", "--mount-proc", os.Args[0], "-test.run=^TestBackendQualificationChild$")
	command.Env = append(os.Environ(), "FSB_BACKEND_CHILD_SOCKET="+socket, "FSB_BACKEND_CHILD_SUM="+sum)
	out, err := command.CombinedOutput()
	require.NoError(t, err, string(out))
}
func TestBackendQualificationChild(t *testing.T) {
	socket := os.Getenv("FSB_BACKEND_CHILD_SOCKET")
	if socket == "" {
		t.Skip("subprocess helper")
	}
	_, err := peerPID(t.Context(), socket)
	require.Error(t, err, "node daemon must be invisible in child PID namespace")
	require.NoError(t, Verify(t.Context(), socket, os.Getenv("FSB_BACKEND_CHILD_SUM")))
}
