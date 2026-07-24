package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
)

const minInotifyInstances = 256

type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

type Option func(*Manager)

type Manager struct {
	profile  Profile
	settings ProfileSettings
	rootDir  string
	tempDir  string
	runner   Runner
	hostOS   string
}

func NewManager(profile Profile, opts ...Option) (*Manager, error) {
	settings, err := profile.Settings()
	if err != nil {
		return nil, err
	}

	rootDir, err := findRootDir()
	if err != nil {
		return nil, err
	}

	manager := &Manager{
		profile:  profile,
		settings: settings,
		rootDir:  rootDir,
		tempDir:  os.TempDir(),
		runner:   execRunner{},
		hostOS:   goruntime.GOOS,
	}
	for _, opt := range opts {
		opt(manager)
	}
	return manager, nil
}

func WithRunner(runner Runner) Option {
	return func(manager *Manager) {
		if runner != nil {
			manager.runner = runner
		}
	}
}

func WithRootDir(rootDir string) Option {
	return func(manager *Manager) {
		if rootDir != "" {
			manager.rootDir = rootDir
		}
	}
}

func WithTempDir(tempDir string) Option {
	return func(manager *Manager) {
		if tempDir != "" {
			manager.tempDir = tempDir
		}
	}
}

func WithHostOS(hostOS string) Option {
	return func(manager *Manager) {
		if hostOS != "" {
			manager.hostOS = hostOS
		}
	}
}

func Require(t testing.TB, profile Profile) *Manager {
	t.Helper()

	manager, err := NewManager(profile)
	if err != nil {
		t.Fatalf("create e2e environment manager: %v", err)
	}
	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure e2e environment for profile %q: %v", profile, err)
	}
	return manager
}

func (m *Manager) Ensure(ctx context.Context) error {
	if m.hostOS != "linux" {
		return fmt.Errorf("e2e profile %q requires a Linux host; run it on the remote development VM or through remote-dev-run", m.profile)
	}
	if err := m.preflight(ctx); err != nil {
		return err
	}
	if err := m.ensureKindCluster(ctx); err != nil {
		return err
	}
	if err := m.ensureKubeSystemReady(ctx); err != nil {
		return err
	}
	if err := m.ensureRuntime(ctx); err != nil {
		return err
	}
	if err := m.ensureBaseImages(ctx); err != nil {
		return err
	}
	if err := m.deployFastSandbox(ctx); err != nil {
		return err
	}
	return nil
}

func (m *Manager) FastctlBinaryPath() string {
	return filepath.Join(m.rootDir, "bin", "fastctl")
}

func (m *Manager) BuildFastctl(ctx context.Context) (string, error) {
	binaryPath := m.FastctlBinaryPath()
	if _, err := m.run(ctx, "go", "build", "-o", binaryPath, "./cmd/fastctl"); err != nil {
		return "", err
	}
	return binaryPath, nil
}

func (m *Manager) preflight(ctx context.Context) error {
	for _, command := range []string{"docker", "kind", "kubectl", "make", "go"} {
		if _, err := m.run(ctx, "sh", "-c", "command -v "+command); err != nil {
			return fmt.Errorf("missing required command %q: %w", command, err)
		}
	}
	if err := m.preflightInotify(ctx); err != nil {
		return err
	}
	if err := m.prepareRuntimeDependencies(ctx); err != nil {
		return err
	}
	if err := m.preflightRuntime(ctx); err != nil {
		return err
	}
	return nil
}

func (m *Manager) prepareRuntimeDependencies(ctx context.Context) error {
	if m.settings.Runtime != RuntimeGVisor {
		return nil
	}
	return m.ensureGVisorHostBinaries(ctx)
}

func (m *Manager) ensureGVisorHostBinaries(ctx context.Context) error {
	runscPath := gvisorRunscBin()
	shimPath := gvisorShimBin()
	if _, err := m.run(ctx, "sh", "-c", "test -x "+shellQuote(runscPath)+" -a -x "+shellQuote(shimPath)); err == nil {
		return nil
	}
	cacheDir := filepath.Dir(runscPath)
	if filepath.Dir(shimPath) != cacheDir {
		return fmt.Errorf("GVISOR_RUNSC_BIN and GVISOR_SHIM_BIN must use one cache directory for automatic installation")
	}
	if _, err := m.run(ctx, "mkdir", "-p", cacheDir); err != nil {
		return err
	}
	for _, name := range []string{"runsc", "containerd-shim-runsc-v1"} {
		binary := filepath.Join(cacheDir, name)
		checksum := binary + ".sha512"
		if _, err := m.run(ctx, "wget", "-q", "--show-progress", gvisorReleaseURL()+"/"+name, "-O", binary); err != nil {
			return err
		}
		if _, err := m.run(ctx, "wget", "-q", "--show-progress", gvisorReleaseURL()+"/"+name+".sha512", "-O", checksum); err != nil {
			return err
		}
	}
	if _, err := m.run(ctx, "sh", "-c", "cd "+shellQuote(cacheDir)+" && sha512sum -c runsc.sha512 -c containerd-shim-runsc-v1.sha512"); err != nil {
		return err
	}
	_, err := m.run(ctx, "chmod", "a+rx", runscPath, shimPath)
	return err
}

func (m *Manager) preflightRuntime(ctx context.Context) error {
	switch {
	case m.settings.Runtime == RuntimeGVisor:
		for _, binary := range []struct {
			name string
			path string
			env  string
		}{
			{name: "runsc", path: gvisorRunscBin(), env: "GVISOR_RUNSC_BIN"},
			{name: "containerd-shim-runsc-v1", path: gvisorShimBin(), env: "GVISOR_SHIM_BIN"},
		} {
			if _, err := m.run(ctx, "sh", "-c", "test -x "+shellQuote(binary.path)); err != nil {
				return fmt.Errorf("gVisor %s binary is not executable at %s; install it or set %s: %w", binary.name, binary.path, binary.env, err)
			}
		}
	case isKataRuntime(m.settings.Runtime):
		for _, path := range kataRequiredHostPaths() {
			if _, err := m.run(ctx, "sh", "-c", "test -e "+shellQuote(path)); err != nil {
				return fmt.Errorf("Kata profile %q requires host path %s; prepare KVM/nested virtualization devices on the remote VM: %w", m.profile, path, err)
			}
		}
	}
	return nil
}

func (m *Manager) preflightInotify(ctx context.Context) error {
	output, err := m.run(ctx, "cat", "/proc/sys/fs/inotify/max_user_instances")
	if err != nil {
		return nil
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil {
		return nil
	}
	if limit < minInotifyInstances {
		return fmt.Errorf("fs.inotify.max_user_instances=%d is too low for kind-based e2e; set it to at least %d, for example: sudo sysctl -w fs.inotify.max_user_instances=%d", limit, minInotifyInstances, minInotifyInstances)
	}
	return nil
}

func (m *Manager) ensureKindCluster(ctx context.Context) error {
	output, err := m.run(ctx, "kind", "get", "clusters")
	if err != nil {
		return err
	}

	if !hasLine(string(output), m.settings.ClusterName) {
		args := []string{"create", "cluster", "--name", m.settings.ClusterName, "--image", m.settings.KindImage}
		configPath, err := m.kindConfigForCreate()
		if err != nil {
			return err
		}
		if configPath != "" {
			args = append(args, "--config", configPath)
		}
		if _, err := m.run(ctx, "kind", args...); err != nil {
			return err
		}
	}

	_, err = m.run(ctx, "kubectl", "config", "use-context", "kind-"+m.settings.ClusterName)
	return err
}

func (m *Manager) ensureBaseImages(ctx context.Context) error {
	if _, err := m.run(ctx, "docker", "image", "inspect", "alpine:latest"); err != nil {
		if _, err := m.run(ctx, "docker", "pull", "alpine:latest"); err != nil {
			return err
		}
	}
	_, err := m.run(ctx, "kind", "load", "docker-image", "alpine:latest", "--name", m.settings.ClusterName)
	return err
}

func (m *Manager) kindConfigForCreate() (string, error) {
	if m.settings.KindConfig == "" {
		return "", nil
	}

	sourcePath := filepath.Join(m.rootDir, m.settings.KindConfig)
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", fmt.Errorf("read kind config %s: %w", m.settings.KindConfig, err)
	}

	if !strings.Contains(string(data), "${") {
		return m.settings.KindConfig, nil
	}

	if err := os.MkdirAll(m.tempDir, 0755); err != nil {
		return "", fmt.Errorf("create kind config temp dir: %w", err)
	}
	renderedPath := filepath.Join(m.tempDir, m.settings.ClusterName+"-kind.yaml")
	rendered := os.Expand(string(data), func(key string) string {
		switch key {
		case "GVISOR_KIND_CLUSTER":
			return m.settings.ClusterName
		case "GVISOR_KIND_IMAGE":
			return m.settings.KindImage
		case "GVISOR_RUNSC_BIN":
			return gvisorRunscBin()
		case "GVISOR_SHIM_BIN":
			return gvisorShimBin()
		case "PWD":
			return m.rootDir
		default:
			return os.Getenv(key)
		}
	})
	if err := os.WriteFile(renderedPath, []byte(rendered), 0644); err != nil {
		return "", fmt.Errorf("write rendered kind config: %w", err)
	}
	return renderedPath, nil
}

func (m *Manager) ensureKubeSystemReady(ctx context.Context) error {
	steps := [][]string{
		{"rollout", "status", "ds/kube-proxy", "-n", "kube-system", "--timeout=120s"},
		{"rollout", "status", "deployment/coredns", "-n", "kube-system", "--timeout=120s"},
	}
	for _, args := range steps {
		if _, err := m.run(ctx, "kubectl", args...); err != nil {
			return fmt.Errorf("kube-system is not healthy: %w\n%s", err, m.kubeSystemDiagnostics(ctx))
		}
	}
	return nil
}

func (m *Manager) ensureRuntime(ctx context.Context) error {
	switch m.settings.Runtime {
	case RuntimeContainer:
		return nil
	case RuntimeGVisor:
		return m.ensureGVisorRuntime(ctx)
	case RuntimeKataQemu, RuntimeKataClh, RuntimeKataFc:
		return m.ensureKataRuntime(ctx)
	default:
		return nil
	}
}

func (m *Manager) ensureGVisorRuntime(ctx context.Context) error {
	nodes, err := m.kindNodeContainers(ctx)
	if err != nil {
		return err
	}

	for _, node := range nodes {
		if _, err := m.run(ctx, "docker", "exec", node, "sh", "-c", gvisorRunscTomlMatchesScript()); err == nil {
			continue
		}
		if _, err := m.run(ctx, "docker", "exec", node, "sh", "-c", gvisorRunscTomlScript()); err != nil {
			return err
		}
		if _, err := m.run(ctx, "docker", "exec", node, "pkill", "-HUP", "containerd"); err != nil {
			return err
		}
		if _, err := m.run(ctx, "docker", "exec", node, "sh", "-c", containerdReadyScript()); err != nil {
			return fmt.Errorf("containerd did not become ready on kind node %q after configuring gVisor: %w", node, err)
		}
	}
	if _, err := m.run(ctx, "sh", "-c", kubeSystemDeploymentsAvailableScript()); err != nil {
		return fmt.Errorf("kube-system deployments did not recover after configuring gVisor: %w", err)
	}
	_, err = m.run(ctx, "kubectl", "apply", "-f", "test/e2e/manifests/runtimeclass/gvisor.yaml")
	return err
}

func (m *Manager) ensureKataRuntime(ctx context.Context) error {
	nodes, err := m.kindNodeContainers(ctx)
	if err != nil {
		return err
	}

	restarted := false
	for _, node := range nodes {
		if err := m.ensureKataInstalled(ctx, node); err != nil {
			return err
		}
		if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataFastSandboxConfigScript()); err != nil {
			return err
		}
		if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataContainerdConfiguredScript()); err != nil {
			if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataConfigureContainerdScript()); err != nil {
				return err
			}
			if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataRestartContainerdScript()); err != nil {
				return err
			}
			if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", containerdReadyScript()); err != nil {
				return fmt.Errorf("containerd did not become ready on kind node %q after configuring Kata: %w", node, err)
			}
			restarted = true
		}
		if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataPauseImageScript()); err != nil {
			return err
		}
	}
	if restarted {
		if _, err := m.run(ctx, "sh", "-c", kubeSystemDeploymentsAvailableScript()); err != nil {
			return fmt.Errorf("kube-system deployments did not recover after configuring Kata: %w", err)
		}
	}
	_, err = m.run(ctx, "kubectl", "apply", "-f", "test/e2e/manifests/runtimeclass/kata.yaml")
	return err
}

func (m *Manager) kindNodeContainers(ctx context.Context) ([]string, error) {
	output, err := m.run(ctx, "docker", "ps", "--filter", "name="+m.settings.ClusterName+"-", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	nodes := nonEmptyLines(string(output))
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no kind node containers found for cluster %q", m.settings.ClusterName)
	}
	return nodes, nil
}

func (m *Manager) ensureKataInstalled(ctx context.Context, node string) error {
	if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataInstalledScript()); err == nil {
		return nil
	}
	if err := m.ensureKataTarball(ctx); err != nil {
		return err
	}
	if _, err := m.run(ctx, "docker", "cp", kataDataFile(), node+":/root/kata.tar.zst"); err != nil {
		return err
	}
	_, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataInstallScript())
	return err
}

func (m *Manager) ensureKataTarball(ctx context.Context) error {
	if _, err := m.run(ctx, "mkdir", "-p", kataDataDir(), kataCacheDir()); err != nil {
		return err
	}
	if _, err := m.run(ctx, "sh", "-c", "test -f "+shellQuote(kataCacheFile())); err != nil {
		if _, err := m.run(ctx, "wget", "-q", "--show-progress", kataURL(), "-O", kataCacheFile()); err != nil {
			return err
		}
	}
	_, err := m.run(ctx, "cp", kataCacheFile(), kataDataFile())
	return err
}

func containerdReadyScript() string {
	return "for i in $(seq 1 60); do ctr version >/dev/null 2>&1 && exit 0; sleep 1; done; ctr version"
}

func kubeSystemDeploymentsAvailableScript() string {
	return "for i in $(seq 1 30); do kubectl wait --for=condition=Available --all deployments -n kube-system --timeout=10s && exit 0; sleep 2; done; kubectl wait --for=condition=Available --all deployments -n kube-system --timeout=120s"
}

func gvisorRunscTomlScript() string {
	return `mkdir -p /etc/containerd /var/log/runsc
cat > /etc/containerd/runsc.toml <<'EOF'
` + gvisorRunscTomlContent() + `
EOF`
}

func gvisorRunscTomlMatchesScript() string {
	return `cat <<'EOF' | cmp -s - /etc/containerd/runsc.toml
` + gvisorRunscTomlContent() + `
EOF`
}

func gvisorRunscTomlContent() string {
	return `log_path = "/var/log/runsc/%ID%/shim.log"
log_level = "debug"

[runsc_config]
platform = "ptrace"
network = "host"
debug = "true"
debug-log = "/var/log/runsc/%ID%/gvisor.%COMMAND%.log"`
}

func isKataRuntime(runtime RuntimeKind) bool {
	switch runtime {
	case RuntimeKataQemu, RuntimeKataClh, RuntimeKataFc:
		return true
	default:
		return false
	}
}

func kataRequiredHostPaths() []string {
	return []string{
		"/dev/kvm",
		"/sys/devices/virtual/misc/kvm",
		"/dev/vhost-vsock",
		"/sys/devices/virtual/misc/vhost-vsock",
		"/dev/vhost-net",
		"/sys/devices/virtual/misc/vhost-net",
		"/dev/net/tun",
		"/dev/shm",
	}
}

func kataInstalledScript() string {
	return "test -x /opt/kata/bin/containerd-shim-kata-v2 && test -f /opt/kata/share/defaults/kata-containers/configuration-clh.toml && test -f /opt/kata/share/defaults/kata-containers/configuration-qemu.toml && test -f /opt/kata/share/defaults/kata-containers/configuration-fc.toml"
}

func kataFastSandboxConfigScript() string {
	return `set -e
config=/opt/kata/share/defaults/kata-containers/configuration-clh.toml
sed -i 's/^sandbox_cgroup_only = false$/sandbox_cgroup_only = true/' "$config"
grep -q '^sandbox_cgroup_only = true$' "$config"`
}

func kataInstallScript() string {
	return `set -e
if ` + kataInstalledScript() + `; then
  exit 0
fi
if ! command -v zstd >/dev/null 2>&1; then
  if [ -f /etc/apt/sources.list.d/debian.sources ]; then
    sed -i "s|http://deb.debian.org|https://mirrors.aliyun.com|g" /etc/apt/sources.list.d/debian.sources
  elif [ -f /etc/apt/sources.list ]; then
    sed -i "s|http://deb.debian.org|https://mirrors.aliyun.com|g" /etc/apt/sources.list
    sed -i "s|http://security.debian.org|https://mirrors.aliyun.com|g" /etc/apt/sources.list 2>/dev/null || true
  fi
  for i in 1 2 3; do
    timeout 60 apt-get update -qq && break
    sleep 2
  done
  apt-get install -y -qq zstd
fi
zstd -df /root/kata.tar.zst -o /root/kata.tar
tar -xf /root/kata.tar -C /
rm -f /root/kata.tar.zst /root/kata.tar
test -x /opt/kata/bin/containerd-shim-kata-v2`
}

func kataContainerdConfiguredScript() string {
	return "grep -q 'runtimes.kata-clh' /etc/containerd/config.toml && grep -q 'runtimes.kata-qemu' /etc/containerd/config.toml && grep -q 'runtimes.kata-fc' /etc/containerd/config.toml"
}

func kataConfigureContainerdScript() string {
	return `set -e
` + kataContainerdConfiguredScript() + ` && exit 0
cp /etc/containerd/config.toml /etc/containerd/config.toml.bak 2>/dev/null || true
cat >> /etc/containerd/config.toml <<'EOF'
` + kataContainerdConfig() + `
EOF`
}

func kataContainerdConfig() string {
	return `
# Kata Containers runtime configurations
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-clh]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-clh.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-clh.toml"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-fc.toml"`
}

func kataRestartContainerdScript() string {
	return "systemctl restart containerd || pkill -HUP containerd"
}

func kataPauseImageScript() string {
	return `if ! ctr -n k8s.io image ls | grep -q "pause.*3.8"; then
  if ctr -n k8s.io image ls | grep -q "pause.*3.10"; then
    ctr -n k8s.io image tag registry.k8s.io/pause:3.10 registry.k8s.io/pause:3.8
  fi
fi`
}

func kataVersion() string {
	return firstNonEmpty(os.Getenv("KATA_VERSION"), "3.27.0")
}

func kataArch() string {
	return firstNonEmpty(os.Getenv("KATA_ARCH"), "amd64")
}

func kataTarballName() string {
	return "kata-static-" + kataVersion() + "-" + kataArch() + ".tar.zst"
}

func kataURL() string {
	return "https://github.com/kata-containers/kata-containers/releases/download/" + kataVersion() + "/" + kataTarballName()
}

func kataDataDir() string {
	return firstNonEmpty(os.Getenv("DATA_DIR"), filepath.Join(homeDir(), "data"))
}

func kataCacheDir() string {
	return firstNonEmpty(os.Getenv("KATA_CACHE_DIR"), filepath.Join(homeDir(), ".cache"))
}

func kataDataFile() string {
	return filepath.Join(kataDataDir(), kataTarballName())
}

func kataCacheFile() string {
	return filepath.Join(kataCacheDir(), kataTarballName())
}

func homeDir() string {
	if value := os.Getenv("HOME"); value != "" {
		return value
	}
	if value, err := os.UserHomeDir(); err == nil && value != "" {
		return value
	}
	return "."
}

func (m *Manager) kubeSystemDiagnostics(ctx context.Context) string {
	var b strings.Builder
	if output, err := m.runner.Run(ctx, m.rootDir, "kubectl", "get", "pods", "-n", "kube-system", "-o", "wide"); err == nil {
		b.WriteString("kube-system pods:\n")
		b.Write(output)
	}
	if output, err := m.runner.Run(ctx, m.rootDir, "kubectl", "logs", "ds/kube-proxy", "-n", "kube-system", "--tail=80"); err == nil {
		b.WriteString("\nkube-proxy logs:\n")
		b.Write(output)
	}
	return b.String()
}

func (m *Manager) deployFastSandbox(ctx context.Context) error {
	steps := []struct {
		name string
		args []string
	}{
		{name: "make", args: []string{"images", "COMPONENT=core"}},
		{name: "kind", args: []string{"load", "docker-image", "fast-sandbox/controller:dev", "--name", m.settings.ClusterName}},
		{name: "kind", args: []string{"load", "docker-image", "fast-sandbox/fastlet:dev", "--name", m.settings.ClusterName}},
		{name: "kind", args: []string{"load", "docker-image", "fast-sandbox/fastlet-proxy:dev", "--name", m.settings.ClusterName}},
		{name: "kind", args: []string{"load", "docker-image", "fast-sandbox/sandbox-proxy:dev", "--name", m.settings.ClusterName}},
		{name: "kind", args: []string{"load", "docker-image", "fast-sandbox/janitor:dev", "--name", m.settings.ClusterName}},
		{name: "kubectl", args: []string{"apply", "-k", "config/crd"}},
		{name: "kubectl", args: []string{"wait", "--for=condition=Established", "crd/sandboxes.sandbox.fast.io", "--timeout=30s"}},
		{name: "kubectl", args: []string{"wait", "--for=condition=Established", "crd/sandboxpools.sandbox.fast.io", "--timeout=30s"}},
		{name: "kubectl", args: []string{"apply", "-f", "config/rbac/base.yaml"}},
		{name: "kubectl", args: []string{"apply", "-f", "config/dev/route-keys.yaml"}},
		{name: "kubectl", args: []string{"apply", "-f", "config/manager/controller.yaml"}},
		{name: "kubectl", args: []string{"rollout", "restart", "deployment/fast-sandbox-controller"}},
		{name: "kubectl", args: []string{"rollout", "status", "deployment/fast-sandbox-controller", "--timeout=120s"}},
		{name: "kubectl", args: []string{"rollout", "restart", "deployment/fast-sandbox-fastpath"}},
		{name: "kubectl", args: []string{"rollout", "status", "deployment/fast-sandbox-fastpath", "--timeout=120s"}},
		{name: "kubectl", args: []string{"rollout", "restart", "deployment/fast-sandbox-proxy"}},
		{name: "kubectl", args: []string{"rollout", "status", "deployment/fast-sandbox-proxy", "--timeout=120s"}},
		{name: "kubectl", args: []string{"apply", "-f", "config/janitor/janitor.yaml"}},
		{name: "kubectl", args: []string{"rollout", "restart", "ds/fast-sandbox-janitor"}},
		{name: "kubectl", args: []string{"rollout", "status", "ds/fast-sandbox-janitor", "--timeout=60s"}},
	}

	for _, step := range steps {
		if _, err := m.run(ctx, step.name, step.args...); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := m.runner.Run(ctx, m.rootDir, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w\n%s", commandString(name, args...), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func findRootDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd, nil
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return "", fmt.Errorf("could not find repository root from %s", wd)
		}
		wd = parent
	}
}

func hasLine(output, want string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func nonEmptyLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func gvisorRunscBin() string {
	return firstNonEmpty(os.Getenv("GVISOR_RUNSC_BIN"), filepath.Join(gvisorCacheDir(), "runsc"))
}

func gvisorShimBin() string {
	return firstNonEmpty(os.Getenv("GVISOR_SHIM_BIN"), filepath.Join(gvisorCacheDir(), "containerd-shim-runsc-v1"))
}

func gvisorCacheDir() string {
	return filepath.Join(homeDir(), ".cache", "fast-sandbox", "gvisor", gvisorRelease(), gvisorArch())
}

func gvisorRelease() string {
	return firstNonEmpty(os.Getenv("GVISOR_RELEASE"), "latest")
}

func gvisorArch() string {
	if value := os.Getenv("GVISOR_ARCH"); value != "" {
		return value
	}
	switch goruntime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return goruntime.GOARCH
	}
}

func gvisorReleaseURL() string {
	return "https://storage.googleapis.com/gvisor/releases/release/" + gvisorRelease() + "/" + gvisorArch()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("_-./:", r):
		default:
			return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		}
	}
	return value
}

func commandString(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}
