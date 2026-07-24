package supervisor

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	infracatalog "fast-sandbox/internal/catalog/infra"

	"github.com/stretchr/testify/require"
)

func TestSupervisorPreservesUserExitCodeAndRunsComponentInParallel(t *testing.T) {
	root := t.TempDir()
	componentMarker := filepath.Join(root, "component")
	userMarker := filepath.Join(root, "user")
	config := Config{Version: ConfigVersion, SandboxUID: "uid-a", Components: []Component{{
		Name: "component", Command: "/bin/sh", Args: []string{"-c", "sleep 0.2; touch " + componentMarker + "; exec sleep 30"},
		RestartPolicy: infracatalog.RestartNever, Readiness: Readiness{Type: infracatalog.ProbeNone},
	}}}
	supervisor := NewSupervisor(io.Discard, io.Discard)
	start := time.Now()
	exitCode, err := supervisor.Run(context.Background(), config, []string{"/bin/sh", "-c", "touch " + userMarker + "; exit 7"})
	require.NoError(t, err)
	require.Equal(t, 7, exitCode)
	require.Less(t, time.Since(start), 2*time.Second)
	require.FileExists(t, userMarker)
	_, statErr := os.Stat(componentMarker)
	require.ErrorIs(t, statErr, os.ErrNotExist, "parallel component must not gate user startup")
}

func TestSupervisorStartsPreUserComponentBeforeUser(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "component")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	readinessAddress := listener.Addr().String()
	require.NoError(t, listener.Close())
	config := Config{Version: ConfigVersion, SandboxUID: "uid-a", Components: []Component{{
		Name: "component", Command: os.Args[0], Args: []string{"-test.run=TestSandboxInitHelperProcess", "--"},
		Env:             map[string]string{"GO_WANT_SANDBOX_INIT_HELPER": "1", "SANDBOX_INIT_MARKER": marker, "SANDBOX_INIT_LISTEN": readinessAddress},
		StartBeforeUser: true, RestartPolicy: infracatalog.RestartNever,
		Readiness: Readiness{Type: infracatalog.ProbeTCP, Address: readinessAddress, Timeout: 2 * time.Second, Interval: 10 * time.Millisecond},
	}}}
	supervisor := NewSupervisor(io.Discard, io.Discard)
	exitCode, err := supervisor.Run(context.Background(), config, []string{"/bin/sh", "-c", "test -f " + marker})
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
}

func TestSandboxInitHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SANDBOX_INIT_HELPER") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("SANDBOX_INIT_MARKER"), []byte("ready"), 0600); err != nil {
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", os.Getenv("SANDBOX_INIT_LISTEN"))
	if err != nil {
		os.Exit(3)
	}
	defer listener.Close()
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			os.Exit(4)
		}
		_ = connection.Close()
	}
}

func TestSupervisorUsesRequiredFlagForStartFailure(t *testing.T) {
	base := Config{Version: ConfigVersion, SandboxUID: "uid-a", Components: []Component{{
		Name: "missing", Command: "/definitely/missing/infra-component", RestartPolicy: infracatalog.RestartNever,
		Readiness: Readiness{Type: infracatalog.ProbeNone},
	}}}

	optional := base
	optional.Components = append([]Component(nil), base.Components...)
	code, err := NewSupervisor(io.Discard, io.Discard).Run(context.Background(), optional, []string{"/bin/sh", "-c", "exit 0"})
	require.NoError(t, err)
	require.Zero(t, code)

	required := base
	required.Components = append([]Component(nil), base.Components...)
	required.Components[0].Required = true
	_, err = NewSupervisor(io.Discard, io.Discard).Run(context.Background(), required, []string{"/bin/sh", "-c", "exit 0"})
	require.ErrorContains(t, err, "start component missing")
}

func TestConfigOrdersDependenciesAndRejectsCycles(t *testing.T) {
	components := []Component{
		{Name: "consumer", Command: "/bin/true", DependsOn: []string{"provider"}},
		{Name: "provider", Command: "/bin/true"},
	}
	ordered, err := orderedComponents(components)
	require.NoError(t, err)
	require.Equal(t, []string{"provider", "consumer"}, []string{ordered[0].Name, ordered[1].Name})

	config := Config{Version: ConfigVersion, SandboxUID: "uid-a", Components: []Component{
		{Name: "a", Command: "/bin/true", DependsOn: []string{"b"}},
		{Name: "b", Command: "/bin/true", DependsOn: []string{"a"}},
	}}
	require.ErrorContains(t, config.Validate(), "cycle")
}

func TestUserProcessAttributesPreserveOriginalOCICredential(t *testing.T) {
	attributes := userProcessAttributes(&UserCredential{UID: 1000, GID: 1001, AdditionalGIDs: []uint32{10, 20}})
	require.NotNil(t, attributes.Credential)
	require.Equal(t, uint32(1000), attributes.Credential.Uid)
	require.Equal(t, uint32(1001), attributes.Credential.Gid)
	require.Equal(t, []uint32{10, 20}, attributes.Credential.Groups)
}
