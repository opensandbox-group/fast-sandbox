package oci_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNodeDaemonAttestationDoesNotStopStorage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("node launcher requires Linux flock and process signals")
	}
	for _, mode := range []string{"success", "attestation-retry", "attestation-failure", "socket-timeout", "daemon-exit"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			socketDir, tools := filepath.Join(dir, "socket"), filepath.Join(dir, "tools")
			require.NoError(t, os.MkdirAll(socketDir, 0700))
			require.NoError(t, os.MkdirAll(tools, 0700))
			// Rewrite only installation paths; exercise the real launcher logic
			// with disposable processes, including its traps and daemon lock.
			raw, err := os.ReadFile("node-daemon.sh")
			require.NoError(t, err)
			script := strings.NewReplacer("/run/fast-sandbox/overlaybd", socketDir, "/opt/fast-sandbox/oci/current/bin", tools).Replace(string(raw))
			launcher := filepath.Join(dir, "launcher.sh")
			require.NoError(t, os.WriteFile(launcher, []byte(script), 0600))
			for name, contents := range map[string]string{
				"overlaybd-ublkd": `#!/bin/sh
echo $$ > "$TEST_NODE_DIR/daemon.pid"
if [ "$TEST_NODE_MODE" = daemon-exit ]; then exit 7; fi
if [ -S "$TEST_NODE_DIR/socket/ready.sock" ]; then
  mv "$TEST_NODE_DIR/socket/ready.sock" "$TEST_NODE_DIR/socket/ublkd.sock"
fi
exec /bin/sleep 60
`,
				"fsb-vm": `#!/bin/sh
echo attempt >> "$TEST_NODE_DIR/attempts"
if [ "$TEST_NODE_MODE" = attestation-retry ] && [ "$(wc -l < "$TEST_NODE_DIR/attempts")" -lt 3 ]; then exit 9; fi
echo record > "$TEST_NODE_DIR/socket/ublkd.sock.qualification.json"
if [ "$TEST_NODE_MODE" = attestation-failure ]; then exit 9; fi
`,
				"sleep": "#!/bin/sh\nexit 0\n",
			} {
				require.NoError(t, os.WriteFile(filepath.Join(tools, name), []byte(contents), 0755))
			}
			if mode == "success" || mode == "attestation-retry" || mode == "attestation-failure" {
				// Startup removes old sockets, so the fake daemon publishes this one.
				listener, err := net.Listen("unix", filepath.Join(socketDir, "ready.sock"))
				require.NoError(t, err)
				defer listener.Close()
			}
			// Fast polling must still yield to the daemon before the timeout.
			if mode != "socket-timeout" {
				require.NoError(t, os.WriteFile(filepath.Join(tools, "sleep"), []byte("#!/bin/sh\nexec /bin/sleep 0.01\n"), 0755))
			}
			logPath := filepath.Join(dir, "launcher.log")
			log, err := os.Create(logPath)
			require.NoError(t, err)
			defer log.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "sh", launcher)
			command.Env = append(os.Environ(), "TEST_NODE_DIR="+dir, "TEST_NODE_MODE="+mode, "FAST_SANDBOX_STATE_ROOT="+filepath.Join(dir, "state"), "PATH="+tools+":"+os.Getenv("PATH"))
			command.Stdout, command.Stderr = log, log
			require.NoError(t, command.Start())
			t.Cleanup(func() {
				if command.ProcessState != nil {
					return
				}
				_ = command.Process.Kill()
				if raw, err := os.ReadFile(filepath.Join(dir, "daemon.pid")); err == nil {
					if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
				_ = command.Wait()
			})
			if mode == "daemon-exit" {
				var exit *exec.ExitError
				require.ErrorAs(t, command.Wait(), &exit)
				require.Equal(t, 7, exit.ExitCode(), "must propagate the daemon failure, not a timeout")
				require.NoFileExists(t, filepath.Join(socketDir, "ublkd.sock.qualification.json"))
				return
			}
			record := filepath.Join(socketDir, "ublkd.sock.qualification.json")
			require.Eventually(t, func() bool {
				if mode == "success" || mode == "attestation-retry" {
					_, err := os.Stat(record)
					return err == nil
				}
				raw, _ := os.ReadFile(logPath)
				return strings.Contains(string(raw), "RootfsOnly qualification unavailable")
			}, 5*time.Second, 10*time.Millisecond)
			if mode != "success" && mode != "attestation-retry" {
				require.NoFileExists(t, record, "failed attestation must revoke even a partial record")
			}
			if mode == "attestation-retry" || mode == "attestation-failure" {
				attempts, err := os.ReadFile(filepath.Join(dir, "attempts"))
				require.NoError(t, err)
				require.Equal(t, 3, strings.Count(string(attempts), "attempt\n"))
			}
			pidRaw, err := os.ReadFile(filepath.Join(dir, "daemon.pid"))
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
			require.NoError(t, err)
			require.NoError(t, syscall.Kill(pid, 0), "healthy storage must survive qualification failure")
			require.NoError(t, command.Process.Signal(syscall.SIGTERM))
			var exit *exec.ExitError
			require.ErrorAs(t, command.Wait(), &exit)
			require.Equal(t, 143, exit.ExitCode(), "launcher must preserve termination status")
			require.NoFileExists(t, record, "shutdown must revoke attestation")
			require.Error(t, syscall.Kill(pid, 0), "shutdown must stop the owned daemon")
		})
	}
}

func TestNodeToolInstallVerifiesBeforePublishing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("installer uses GNU mv, as in the runtime image")
	}
	source, target := filepath.Join(t.TempDir(), "bundle"), filepath.Join(t.TempDir(), "tools")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "bin"), 0755))
	payload := []byte("#!/bin/sh\necho first\n")
	install := func() ([]byte, error) { return exec.Command("sh", "install-tools.sh", source, target).CombinedOutput() }
	write := func(data []byte) string {
		require.NoError(t, os.WriteFile(filepath.Join(source, "bin/fsb-vm"), data, 0755))
		sums := []byte(fmt.Sprintf("%x  bin/fsb-vm\n", sha256.Sum256(data)))
		require.NoError(t, os.WriteFile(filepath.Join(source, "SHA256SUMS"), sums, 0644))
		revision := fmt.Sprintf("%x", sha256.Sum256(sums))
		require.NoError(t, os.WriteFile(filepath.Join(source, "REVISION"), []byte(revision+"\n"), 0644))
		return revision
	}
	first := write(payload)
	output, err := install()
	require.NoError(t, err, string(output))
	current, err := os.Readlink(filepath.Join(target, "current"))
	require.NoError(t, err)
	require.Equal(t, "releases/"+first, current)
	require.NoError(t, os.WriteFile(filepath.Join(source, "bin/fsb-vm"), []byte("corrupt"), 0755))
	_, err = install()
	require.Error(t, err)
	current, err = os.Readlink(filepath.Join(target, "current"))
	require.NoError(t, err)
	require.Equal(t, "releases/"+first, current)
	second := write([]byte("#!/bin/sh\necho second\n"))
	output, err = install()
	require.NoError(t, err, string(output))
	current, err = os.Readlink(filepath.Join(target, "current"))
	require.NoError(t, err)
	require.Equal(t, "releases/"+second, current)
	retained, err := os.ReadFile(filepath.Join(target, "releases", first, "bin/fsb-vm"))
	require.NoError(t, err)
	require.Equal(t, payload, retained)
}
