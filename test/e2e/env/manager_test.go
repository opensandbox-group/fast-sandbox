package env

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordedCommand struct {
	dir  string
	name string
	args []string
}

type fakeRunner struct {
	outputs  map[string]string
	errs     map[string]error
	commands []recordedCommand
}

func (r *fakeRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, recordedCommand{
		dir:  dir,
		name: name,
		args: append([]string(nil), args...),
	})
	key := commandKey(name, args...)
	if err := r.errs[key]; err != nil {
		return nil, err
	}
	return []byte(r.outputs[key]), nil
}

func TestManagerEnsureBasicCreatesMissingClusterAndDeploys(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"): "other-cluster\n",
		},
		errs: map[string]error{},
	}

	manager, err := NewManager(ProfileBasic,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	assertCommand(t, runner.commands, "kind", "create", "cluster", "--name", "fsb-e2e-basic", "--image", "kindest/node:v1.27.3")
	assertCommand(t, runner.commands, "kubectl", "config", "use-context", "kind-fsb-e2e-basic")
	assertCommand(t, runner.commands, "make", "docker-controller", "docker-fastlet", "docker-fastlet-proxy", "docker-sandbox-proxy", "docker-janitor")
	assertCommand(t, runner.commands, "kind", "load", "docker-image", "fast-sandbox/controller:dev", "--name", "fsb-e2e-basic")
	assertCommand(t, runner.commands, "kubectl", "apply", "-f", "config/crd/")
	assertCommand(t, runner.commands, "kubectl", "rollout", "restart", "deployment/fast-sandbox-controller")
	assertCommand(t, runner.commands, "kubectl", "rollout", "status", "deployment/fast-sandbox-controller", "--timeout=120s")
	assertCommand(t, runner.commands, "kubectl", "rollout", "restart", "deployment/fast-sandbox-fastpath")
	assertCommand(t, runner.commands, "kubectl", "rollout", "status", "deployment/fast-sandbox-fastpath", "--timeout=120s")
	assertCommand(t, runner.commands, "kubectl", "rollout", "status", "deployment/fast-sandbox-proxy", "--timeout=120s")
	assertCommand(t, runner.commands, "kubectl", "rollout", "restart", "ds/fast-sandbox-janitor")
}

func TestManagerEnsureBasicReusesExistingCluster(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"): "fsb-e2e-basic\n",
		},
		errs: map[string]error{},
	}

	manager, err := NewManager(ProfileBasic,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	if hasCommand(runner.commands, "kind", "create", "cluster") {
		t.Fatalf("expected existing cluster to be reused, commands: %#v", runner.commands)
	}
	assertCommand(t, runner.commands, "kubectl", "config", "use-context", "kind-fsb-e2e-basic")
}

func TestManagerEnsureWaitsForKubeSystemBeforeDeploy(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"): "fsb-e2e-basic\n",
		},
		errs: map[string]error{},
	}
	manager, err := NewManager(ProfileBasic,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	kubeProxyIndex := commandIndex(runner.commands, "kubectl", "rollout", "status", "ds/kube-proxy", "-n", "kube-system", "--timeout=120s")
	buildIndex := commandIndex(runner.commands, "make", "docker-controller", "docker-fastlet", "docker-fastlet-proxy", "docker-sandbox-proxy", "docker-janitor")
	if kubeProxyIndex == -1 {
		t.Fatalf("missing kube-proxy rollout wait: %#v", runner.commands)
	}
	if buildIndex == -1 {
		t.Fatalf("missing docker build command: %#v", runner.commands)
	}
	if kubeProxyIndex > buildIndex {
		t.Fatalf("expected kube-system wait before deploy, commands: %#v", runner.commands)
	}
	assertCommand(t, runner.commands, "kubectl", "rollout", "status", "deployment/coredns", "-n", "kube-system", "--timeout=120s")
}

func TestManagerEnsureLoadsAlpineBaseImageBeforeDeploy(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"): "fsb-e2e-basic\n",
		},
		errs: map[string]error{},
	}
	manager, err := NewManager(ProfileBasic,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	loadAlpineIndex := commandIndex(runner.commands, "kind", "load", "docker-image", "alpine:latest", "--name", "fsb-e2e-basic")
	applyIndex := commandIndex(runner.commands, "kubectl", "apply", "-f", "config/crd/")
	if loadAlpineIndex == -1 {
		t.Fatalf("missing alpine kind load command: %#v", runner.commands)
	}
	if applyIndex == -1 {
		t.Fatalf("missing CRD apply command: %#v", runner.commands)
	}
	if loadAlpineIndex > applyIndex {
		t.Fatalf("expected alpine image load before fast-sandbox deploy, commands: %#v", runner.commands)
	}
}

func TestManagerEnsureRendersGVisorKindConfigTemplate(t *testing.T) {
	t.Setenv("GVISOR_RUNSC_BIN", "/opt/gvisor/runsc")
	t.Setenv("GVISOR_SHIM_BIN", "/opt/gvisor/containerd-shim-runsc-v1")

	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"):                                                    "other-cluster\n",
			commandKey("docker", "ps", "--filter", "name=fsb-e2e-gvisor-", "--format", "{{.Names}}"): "fsb-e2e-gvisor-control-plane\n",
		},
		errs: map[string]error{},
	}
	tempDir := t.TempDir()
	manager, err := NewManager(ProfileGVisor,
		WithRunner(runner),
		WithHostOS("linux"),
		WithTempDir(tempDir),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	configPath := filepath.Join(tempDir, "fsb-e2e-gvisor-kind.yaml")
	assertCommand(t, runner.commands, "kind", "create", "cluster", "--name", "fsb-e2e-gvisor", "--image", "kindest/node:v1.31.0", "--config", configPath)

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}
	rendered := string(data)
	for _, want := range []string{
		"name: fsb-e2e-gvisor",
		"image: kindest/node:v1.31.0",
		"hostPath: /opt/gvisor/runsc",
		"hostPath: /opt/gvisor/containerd-shim-runsc-v1",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "${") {
		t.Fatalf("rendered config still contains template variables:\n%s", rendered)
	}
}

func TestManagerEnsureGVisorConfiguresRuntimeOnKindNodes(t *testing.T) {
	t.Setenv("GVISOR_RUNSC_BIN", "/opt/gvisor/runsc")
	t.Setenv("GVISOR_SHIM_BIN", "/opt/gvisor/containerd-shim-runsc-v1")

	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"):                                                    "fsb-e2e-gvisor\n",
			commandKey("docker", "ps", "--filter", "name=fsb-e2e-gvisor-", "--format", "{{.Names}}"): "fsb-e2e-gvisor-control-plane\n",
		},
		errs: map[string]error{
			commandKey("docker", "exec", "fsb-e2e-gvisor-control-plane", "sh", "-c", gvisorRunscTomlMatchesScript()): errors.New("runsc.toml differs"),
		},
	}
	manager, err := NewManager(ProfileGVisor,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	assertCommand(t, runner.commands, "sh", "-c", "test -x /opt/gvisor/runsc")
	assertCommand(t, runner.commands, "sh", "-c", "test -x /opt/gvisor/containerd-shim-runsc-v1")
	assertCommand(t, runner.commands, "docker", "ps", "--filter", "name=fsb-e2e-gvisor-", "--format", "{{.Names}}")
	assertCommandContaining(t, runner.commands, "docker exec fsb-e2e-gvisor-control-plane sh -c", "platform = \"ptrace\"")
	assertCommand(t, runner.commands, "docker", "exec", "fsb-e2e-gvisor-control-plane", "pkill", "-HUP", "containerd")
	assertCommandContaining(t, runner.commands, "docker exec fsb-e2e-gvisor-control-plane sh -c", "ctr version")
	assertCommandContaining(t, runner.commands, "sh -c", "kubectl wait --for=condition=Available --all deployments -n kube-system")
	assertCommand(t, runner.commands, "kubectl", "apply", "-f", "test/e2e/manifests/runtimeclass/gvisor.yaml")

	runtimeClassIndex := commandIndex(runner.commands, "kubectl", "apply", "-f", "test/e2e/manifests/runtimeclass/gvisor.yaml")
	deployIndex := commandIndex(runner.commands, "kubectl", "apply", "-f", "config/crd/")
	if runtimeClassIndex == -1 || deployIndex == -1 {
		t.Fatalf("missing expected apply commands: %#v", runner.commands)
	}
	if runtimeClassIndex > deployIndex {
		t.Fatalf("expected RuntimeClass to be applied before fast-sandbox deployment, commands: %#v", runner.commands)
	}
}

func TestManagerEnsureGVisorSkipsContainerdRestartWhenRunscTomlAlreadyMatches(t *testing.T) {
	t.Setenv("GVISOR_RUNSC_BIN", "/opt/gvisor/runsc")
	t.Setenv("GVISOR_SHIM_BIN", "/opt/gvisor/containerd-shim-runsc-v1")

	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"):                                                    "fsb-e2e-gvisor\n",
			commandKey("docker", "ps", "--filter", "name=fsb-e2e-gvisor-", "--format", "{{.Names}}"): "fsb-e2e-gvisor-control-plane\n",
		},
		errs: map[string]error{},
	}
	manager, err := NewManager(ProfileGVisor,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	if hasCommand(runner.commands, "docker", "exec", "fsb-e2e-gvisor-control-plane", "pkill", "-HUP", "containerd") {
		t.Fatalf("expected matching runsc.toml to skip containerd restart, commands: %#v", runner.commands)
	}
	assertCommand(t, runner.commands, "kubectl", "apply", "-f", "test/e2e/manifests/runtimeclass/gvisor.yaml")
}

func TestManagerEnsureKataInstallsRuntimeAndAppliesRuntimeClasses(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	t.Setenv("DATA_DIR", "/data")
	t.Setenv("KATA_VERSION", "3.27.0")

	const node = "fsb-e2e-kata-control-plane"
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"):                                                  "fsb-e2e-kata\n",
			commandKey("docker", "ps", "--filter", "name=fsb-e2e-kata-", "--format", "{{.Names}}"): node + "\n",
		},
		errs: map[string]error{
			commandKey("docker", "exec", node, "bash", "-c", kataInstalledScript()):            errors.New("kata missing"),
			commandKey("docker", "exec", node, "bash", "-c", kataContainerdConfiguredScript()): errors.New("kata runtime missing"),
		},
	}
	manager, err := NewManager(ProfileKataClh,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	assertCommand(t, runner.commands, "sh", "-c", "test -e /dev/kvm")
	assertCommand(t, runner.commands, "sh", "-c", "test -e /dev/vhost-vsock")
	assertCommand(t, runner.commands, "sh", "-c", "test -e /dev/net/tun")
	assertCommand(t, runner.commands, "mkdir", "-p", "/data", "/home/test/.cache")
	assertCommand(t, runner.commands, "cp", "/home/test/.cache/kata-static-3.27.0-amd64.tar.zst", "/data/kata-static-3.27.0-amd64.tar.zst")
	assertCommand(t, runner.commands, "docker", "cp", "/data/kata-static-3.27.0-amd64.tar.zst", node+":/root/kata.tar.zst")
	assertCommandContaining(t, runner.commands, "docker exec "+node+" bash -c", "tar -xf /root/kata.tar -C /")
	assertCommandContaining(t, runner.commands, "docker exec "+node+" bash -c", "runtimes.kata-clh")
	assertCommandContaining(t, runner.commands, "docker exec "+node+" bash -c", "systemctl restart containerd")
	assertCommandContaining(t, runner.commands, "docker exec "+node+" bash -c", "ctr -n k8s.io image tag")
	assertCommand(t, runner.commands, "kubectl", "apply", "-f", "test/e2e/manifests/runtimeclass/kata.yaml")
}

func TestManagerEnsureKataSkipsInstallAndRestartWhenAlreadyConfigured(t *testing.T) {
	const node = "fsb-e2e-kata-control-plane"
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"):                                                  "fsb-e2e-kata\n",
			commandKey("docker", "ps", "--filter", "name=fsb-e2e-kata-", "--format", "{{.Names}}"): node + "\n",
		},
		errs: map[string]error{},
	}
	manager, err := NewManager(ProfileKataQemu,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	if hasCommand(runner.commands, "docker", "cp") {
		t.Fatalf("expected existing Kata installation to skip tarball copy, commands: %#v", runner.commands)
	}
	if hasCommand(runner.commands, "docker", "exec", node, "bash", "-c", kataRestartContainerdScript()) {
		t.Fatalf("expected existing Kata containerd config to skip restart, commands: %#v", runner.commands)
	}
	assertCommand(t, runner.commands, "kubectl", "apply", "-f", "test/e2e/manifests/runtimeclass/kata.yaml")
}

func TestKataInstallScriptOnlySkipsCompleteInstallation(t *testing.T) {
	script := kataInstallScript()

	if !strings.Contains(script, "if "+kataInstalledScript()+"; then") {
		t.Fatalf("install script should skip only when the full Kata installation is present:\n%s", script)
	}
	if !strings.Contains(script, "zstd -df /root/kata.tar.zst -o /root/kata.tar") {
		t.Fatalf("install script should overwrite stale extraction output:\n%s", script)
	}
}

func TestManagerBuildFastctlInvokesGoBuild(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{},
		errs:    map[string]error{},
	}
	manager, err := NewManager(ProfileBasic,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	binaryPath, err := manager.BuildFastctl(context.Background())
	if err != nil {
		t.Fatalf("BuildFastctl returned error: %v", err)
	}
	if binaryPath != "/repo/bin/fastctl" {
		t.Fatalf("binary path = %q, want /repo/bin/fastctl", binaryPath)
	}

	assertCommand(t, runner.commands, "go", "build", "-o", "/repo/bin/fastctl", "./cmd/fastctl")
}

func TestManagerEnsureRejectsNonLinuxHost(t *testing.T) {
	manager, err := NewManager(ProfileBasic, WithRunner(&fakeRunner{}), WithHostOS("darwin"))
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	err = manager.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected non-Linux host error")
	}
	if !strings.Contains(err.Error(), "requires a Linux host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestManagerPreflightUsesShellCommandLookup(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("kind", "get", "clusters"): "fsb-e2e-basic\n",
		},
		errs: map[string]error{},
	}
	manager, err := NewManager(ProfileBasic,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	assertCommand(t, runner.commands, "sh", "-c", "command -v docker")
	assertCommand(t, runner.commands, "sh", "-c", "command -v kind")
	assertCommand(t, runner.commands, "sh", "-c", "command -v kubectl")
}

func TestManagerEnsureRejectsLowInotifyInstanceLimit(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("cat", "/proc/sys/fs/inotify/max_user_instances"): "128\n",
		},
		errs: map[string]error{},
	}
	manager, err := NewManager(ProfileBasic,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	err = manager.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected low inotify instance limit error")
	}
	if !strings.Contains(err.Error(), "fs.inotify.max_user_instances=128") {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasCommand(runner.commands, "kind", "get", "clusters") {
		t.Fatalf("expected inotify preflight to fail before kind commands: %#v", runner.commands)
	}
}

func TestManagerEnsureWrapsCommandFailure(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{},
		errs: map[string]error{
			commandKey("kind", "get", "clusters"): errors.New("kind unavailable"),
		},
	}
	manager, err := NewManager(ProfileBasic,
		WithRunner(runner),
		WithRootDir("/repo"),
		WithHostOS("linux"),
	)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	err = manager.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected command failure")
	}
	if !strings.Contains(err.Error(), "kind get clusters") {
		t.Fatalf("expected command in error, got: %v", err)
	}
}

func commandKey(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func assertCommand(t *testing.T, commands []recordedCommand, name string, args ...string) {
	t.Helper()
	if !hasCommand(commands, name, args...) {
		t.Fatalf("missing command %q; got %#v", commandKey(name, args...), commands)
	}
}

func assertCommandContaining(t *testing.T, commands []recordedCommand, prefix, contains string) {
	t.Helper()
	for _, command := range commands {
		joined := commandString(command.name, command.args...)
		if strings.HasPrefix(joined, prefix) && strings.Contains(joined, contains) {
			return
		}
	}
	t.Fatalf("missing command with prefix %q containing %q; got %#v", prefix, contains, commands)
}

func hasCommand(commands []recordedCommand, name string, args ...string) bool {
	return commandIndex(commands, name, args...) != -1
}

func commandIndex(commands []recordedCommand, name string, args ...string) int {
	want := commandKey(name, args...)
	for i, command := range commands {
		if commandKey(command.name, command.args...) == want {
			return i
		}
	}
	return -1
}
