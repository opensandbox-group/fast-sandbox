package env

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	minInotifyInstances        = 256
	fastSandboxSystemNamespace = "fast-sandbox-system"
	kindCommand                = "kind"
	kindLoadImageArg           = "docker-image"
	flagName                   = "--name"
	kubectlApplyVerb           = "apply"
	kubectlWaitTimeoutFlag     = "--timeout=120s"
	// kubectlCommand is the kubectl binary used for all cluster operations.
	kubectlCommand           = "kubectl"
	kubectlRolloutSubcommand = "rollout"
	// kubectlStatusSubcommand is the kubectl rollout status subcommand.
	kubectlStatusSubcommand = "status"
	// makeCommand is the local make binary driving the dev images.
	makeCommand        = "make"
	kindLoadSubcommand = "load"
)

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
	progress io.Writer
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

// WithProgressWriter enables human-readable environment progress. Long-running
// subprocess output is streamed to the same writer when the active Runner
// supports streaming.
func WithProgressWriter(writer io.Writer) Option {
	return func(manager *Manager) {
		manager.progress = writer
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

	steps := []struct {
		label string
		run   func(context.Context) error
	}{
		{label: "Checking host prerequisites", run: m.preflight},
		{label: fmt.Sprintf("Preparing kind cluster %s", m.settings.ClusterName), run: m.ensureKindCluster},
		{label: "Waiting for Kubernetes system components", run: m.ensureKubeSystemReady},
		{label: fmt.Sprintf("Preparing runtime %s", m.settings.Runtime), run: m.ensureRuntime},
		{label: "Loading the base Sandbox image", run: m.ensureBaseImages},
		{label: "Building and deploying Fast Sandbox", run: m.deployFastSandbox},
	}
	for index, step := range steps {
		startedAt := time.Now()
		m.progressf("[environment %d/%d] %s...\n", index+1, len(steps), step.label)
		if err := step.run(ctx); err != nil {
			return err
		}
		m.progressf("  done in %s\n", progressDuration(time.Since(startedAt)))
	}
	m.progressf("Environment ready: context=kind-%s\n", m.settings.ClusterName)
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
	for _, command := range []string{"docker", kindCommand, kubectlCommand, makeCommand, "go"} {
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
		if _, err := m.runLive(ctx, "wget", "-q", "--show-progress", gvisorReleaseURL()+"/"+name, "-O", binary); err != nil {
			return err
		}
		if _, err := m.runLive(ctx, "wget", "-q", "--show-progress", gvisorReleaseURL()+"/"+name+".sha512", "-O", checksum); err != nil {
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
		return nil //nolint:nilerr // best-effort guard: skip the check when the sysctl is unreadable
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil {
		return nil //nolint:nilerr // best-effort guard: skip the check when the value is not numeric
	}
	if limit < minInotifyInstances {
		return fmt.Errorf("fs.inotify.max_user_instances=%d is too low for kind-based e2e; set it to at least %d, for example: sudo sysctl -w fs.inotify.max_user_instances=%d", limit, minInotifyInstances, minInotifyInstances)
	}
	return nil
}

func (m *Manager) ensureKindCluster(ctx context.Context) error {
	output, err := m.run(ctx, kindCommand, "get", "clusters")
	if err != nil {
		return err
	}

	if !hasLine(string(output), m.settings.ClusterName) {
		args := []string{"create", "cluster", flagName, m.settings.ClusterName, "--image", m.settings.KindImage}
		configPath, err := m.kindConfigForCreate()
		if err != nil {
			return err
		}
		if configPath != "" {
			args = append(args, "--config", configPath)
		}
		if _, err := m.runLive(ctx, kindCommand, args...); err != nil {
			return err
		}
	}

	_, err = m.run(ctx, kubectlCommand, "config", "use-context", kindCommand+"-"+m.settings.ClusterName)
	return err
}

func (m *Manager) ensureBaseImages(ctx context.Context) error {
	if _, err := m.run(ctx, "docker", "image", "inspect", "alpine:latest"); err != nil {
		if _, err := m.runLive(ctx, "docker", "pull", "alpine:latest"); err != nil {
			return err
		}
	}
	_, err := m.runLive(ctx, kindCommand, kindLoadSubcommand, kindLoadImageArg, "alpine:latest", flagName, m.settings.ClusterName)
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
	if err := os.WriteFile(renderedPath, []byte(rendered), 0600); err != nil {
		return "", fmt.Errorf("write rendered kind config: %w", err)
	}
	return renderedPath, nil
}

func (m *Manager) ensureKubeSystemReady(ctx context.Context) error {
	steps := [][]string{
		{kubectlRolloutSubcommand, kubectlStatusSubcommand, "ds/kube-proxy", "-n", "kube-system", kubectlWaitTimeoutFlag},
		{kubectlRolloutSubcommand, kubectlStatusSubcommand, "deployment/coredns", "-n", "kube-system", kubectlWaitTimeoutFlag},
	}
	for _, args := range steps {
		if _, err := m.runLive(ctx, kubectlCommand, args...); err != nil {
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
	case RuntimeKataQemu, RuntimeKataClh, RuntimeKataFc, RuntimeKataDragonball:
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
	_, err = m.run(ctx, kubectlCommand, kubectlApplyVerb, "-f", "test/e2e/manifests/runtimeclass/gvisor.yaml")
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
		if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataFastSandboxConfigScript(m.settings.Runtime)); err != nil {
			return err
		}
		if m.settings.Runtime == RuntimeKataFc {
			if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataFirecrackerStorageScript()); err != nil {
				return err
			}
		}
		if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataContainerdConfiguredScript(m.settings.Runtime)); err != nil {
			if _, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataConfigureContainerdScript(m.settings.Runtime)); err != nil {
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
	_, err = m.run(ctx, kubectlCommand, kubectlApplyVerb, "-f", "test/e2e/manifests/runtimeclass/kata.yaml")
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
	if err := m.ensureKataArtifactContainer(ctx); err != nil {
		return err
	}
	if _, err := m.runLive(ctx, "sh", "-c", kataPayloadCopyScript(node)); err != nil {
		return err
	}
	_, err := m.run(ctx, "docker", "exec", node, "bash", "-c", kataPayloadVerificationScript())
	return err
}

func (m *Manager) ensureKataArtifactContainer(ctx context.Context) error {
	name := kataArtifactContainerName()
	if _, err := m.run(ctx, "docker", "container", "inspect", name); err == nil {
		return nil
	}
	if _, err := m.runLive(ctx, "docker", "pull", kataDeployImage()); err != nil {
		return err
	}
	_, err := m.run(ctx, "docker", "create", flagName, name, kataDeployImage())
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
	case RuntimeKataQemu, RuntimeKataClh, RuntimeKataFc, RuntimeKataDragonball:
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
	return "test \"$(cat /opt/kata/VERSION 2>/dev/null)\" = " + shellQuote(kataVersion()) + " && test -x /opt/kata/bin/containerd-shim-kata-v2 && test -x /opt/kata/runtime-rs/bin/containerd-shim-kata-v2 && test -f /opt/kata/share/defaults/kata-containers/configuration-clh.toml && test -f /opt/kata/share/defaults/kata-containers/configuration-qemu.toml && test -f /opt/kata/share/defaults/kata-containers/configuration-fc.toml && test -f /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball.toml"
}

func kataPayloadVerificationScript() string {
	return "set -e\n" + kataInstalledScript()
}

func kataFastSandboxConfigScript(runtime RuntimeKind) string {
	script := `set -e
config=/opt/kata/share/defaults/kata-containers/configuration-clh.toml
sed -i 's/^sandbox_cgroup_only = false$/sandbox_cgroup_only = true/' "$config"
grep -q '^sandbox_cgroup_only = true$' "$config"`
	switch runtime {
	case RuntimeKataDragonball:
		return script + `
source=/opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball.toml
target=/opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml
cp "$source" "$target"
sed -i 's#vmlinux-dragonball-experimental.container#vmlinux.container#' "$target"
sed -i 's/^static_sandbox_resource_mgmt = false$/static_sandbox_resource_mgmt = true/' "$target"
sed -i 's/^default_vcpus = 1$/default_vcpus = 2/' "$target"
grep -q '^kernel = "/opt/kata/share/kata-containers/vmlinux.container"$' "$target"
grep -q '^static_sandbox_resource_mgmt = true$' "$target"
grep -q '^default_vcpus = 2$' "$target"`
	case RuntimeKataFc:
		return script + `
source=/opt/kata/share/defaults/kata-containers/configuration-fc.toml
target=/opt/kata/share/defaults/kata-containers/configuration-fc-fast-sandbox.toml
kernel_target=$(basename "$(readlink -f /opt/kata/share/kata-containers/vmlinux.container)")
kernel_config=/opt/kata/share/kata-containers/config-${kernel_target#vmlinux-}
test -f "$kernel_config"
grep -q '^CONFIG_VIRTIO_MMIO=y$' "$kernel_config"
cp "$source" "$target"
grep -q '^kernel = "/opt/kata/share/kata-containers/vmlinux.container"$' "$target"`
	default:
		return script
	}
}

func kataFirecrackerStorageScript() string {
	return `set -e
mkdir -p /var/lib/containerd/blockfile-source /var/lib/containerd/io.containerd.snapshotter.v1.blockfile
scratch=/var/lib/containerd/blockfile-source/scratch.ext4
if [ ! -f "$scratch" ]; then
  truncate -s 512M "$scratch"
  mkfs.ext4 -F -q "$scratch"
fi
for minor in $(seq 0 31); do
  test -e /dev/loop$minor || mknod -m 0660 /dev/loop$minor b 7 $minor
done`
}

func kataBaseContainerdConfiguredScript() string {
	return "grep -q 'runtimes.kata-clh' /etc/containerd/config.toml && grep -q 'runtimes.kata-qemu' /etc/containerd/config.toml && grep -q 'runtimes.kata-fc' /etc/containerd/config.toml && grep -q 'runtimes.kata-dragonball' /etc/containerd/config.toml"
}

func kataContainerdConfiguredScript(runtime RuntimeKind) string {
	configured := kataBaseContainerdConfiguredScript()
	switch runtime {
	case RuntimeKataDragonball:
		return configured + ` &&
grep -A10 'runtimes.kata-dragonball]$' /etc/containerd/config.toml | grep -q 'configuration-dragonball-fast-sandbox.toml' &&
grep -q '^kernel = "/opt/kata/share/kata-containers/vmlinux.container"$' /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml &&
grep -q '^static_sandbox_resource_mgmt = true$' /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml &&
grep -q '^default_vcpus = 2$' /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml`
	case RuntimeKataFc:
		return configured + ` &&
grep -A10 'runtimes.kata-fc]$' /etc/containerd/config.toml | grep -q 'snapshotter = "blockfile"' &&
grep -q 'configuration-fc-fast-sandbox.toml' /etc/containerd/config.toml &&
grep -q 'snapshotter.v1.blockfile' /etc/containerd/config.toml &&
ctr plugins ls | grep -E 'snapshotter.v1[[:space:]]+blockfile[[:space:]].*[[:space:]]ok[[:space:]]*$'`
	default:
		return configured
	}
}

func kataConfigureContainerdScript(runtime RuntimeKind) string {
	script := `set -e
cp /etc/containerd/config.toml /etc/containerd/config.toml.bak 2>/dev/null || true`
	var scriptSb602 strings.Builder
	for _, runtimeName := range []RuntimeKind{RuntimeKataClh, RuntimeKataQemu, RuntimeKataFc, RuntimeKataDragonball} {
		scriptSb602.WriteString(`
if ! grep -q 'runtimes.` + string(runtimeName) + `' /etc/containerd/config.toml; then
  cat >> /etc/containerd/config.toml <<'EOF'
` + kataRuntimeContainerdConfig(runtimeName) + `
EOF
fi`)
	}
	script += scriptSb602.String()
	if runtime == RuntimeKataFc {
		script += `
if ! grep -A10 'runtimes.kata-fc]$' /etc/containerd/config.toml | grep -q 'snapshotter = "blockfile"'; then
  sed -i '/runtimes.kata-fc]$/a\  snapshotter = "blockfile"' /etc/containerd/config.toml
fi
sed -i 's#ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-fc.toml"#ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-fc-fast-sandbox.toml"#' /etc/containerd/config.toml
if ! grep -q 'snapshotter.v1.blockfile' /etc/containerd/config.toml; then
  cat >> /etc/containerd/config.toml <<'EOF'
` + kataBlockfileContainerdConfig() + `
EOF
fi`
	}
	if runtime == RuntimeKataDragonball {
		script += `
sed -i 's#ConfigPath = "/opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball.toml"#ConfigPath = "/opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml"#' /etc/containerd/config.toml`
	}
	return script
}

func kataContainerdConfig() string { //nolint:unused // exercised by manager_test.go; linter skips test files (run.tests=false)
	return "\n# Kata Containers runtime configurations\n" +
		kataRuntimeContainerdConfig(RuntimeKataClh) + "\n" +
		kataRuntimeContainerdConfig(RuntimeKataQemu) + "\n" +
		kataRuntimeContainerdConfig(RuntimeKataFc) + "\n" +
		kataRuntimeContainerdConfig(RuntimeKataDragonball) + "\n" +
		kataBlockfileContainerdConfig()
}

func kataRuntimeContainerdConfig(runtime RuntimeKind) string {
	switch runtime {
	case RuntimeKataClh:
		return `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-clh]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-clh.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-clh.toml"`
	case RuntimeKataQemu:
		return `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml"`
	case RuntimeKataFc:
		return `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  snapshotter = "blockfile"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-fc-fast-sandbox.toml"`
	case RuntimeKataDragonball:
		return `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-dragonball]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/runtime-rs/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-dragonball.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml"`
	default:
		return ""
	}
}

func kataBlockfileContainerdConfig() string {
	return `[plugins."io.containerd.snapshotter.v1.blockfile"]
  root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.blockfile"
  scratch_file = "/var/lib/containerd/blockfile-source/scratch.ext4"
  fs_type = "ext4"
  mount_options = ["loop", "direct-io", "sync"]`
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
	return firstNonEmpty(os.Getenv("KATA_VERSION"), "3.31.0")
}

func kataDeployImage() string {
	if configured := strings.TrimSpace(os.Getenv("KATA_DEPLOY_IMAGE")); configured != "" {
		return configured
	}
	if kataVersion() == "3.31.0" {
		return "quay.io/kata-containers/kata-deploy:3.31.0@sha256:643471067559cbde3258a62eb60840ce435383d9a6dae7a803bd9418eb81be8b"
	}
	return "quay.io/kata-containers/kata-deploy:" + kataVersion()
}

func kataArtifactContainerName() string {
	version := strings.NewReplacer(".", "-", "/", "-", ":", "-").Replace(kataVersion())
	digest := sha256.Sum256([]byte(kataDeployImage()))
	return fmt.Sprintf("fast-sandbox-kata-artifacts-%s-%x", version, digest[:6])
}

func kataPayloadCopyScript(node string) string {
	source := kataArtifactContainerName() + ":/opt/kata-artifacts/opt/kata"
	destination := node + ":/opt"
	return "docker cp " + shellQuote(source) + " - | docker cp - " + shellQuote(destination)
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
	if output, err := m.runner.Run(ctx, m.rootDir, kubectlCommand, "get", "pods", "-n", "kube-system", "-o", "wide"); err == nil {
		b.WriteString("kube-system pods:\n")
		b.Write(output)
	}
	if output, err := m.runner.Run(ctx, m.rootDir, kubectlCommand, "logs", "ds/kube-proxy", "-n", "kube-system", "--tail=80"); err == nil {
		b.WriteString("\nkube-proxy logs:\n")
		b.Write(output)
	}
	return b.String()
}

func (m *Manager) deployFastSandbox(ctx context.Context) error {
	steps := []struct {
		label  string
		name   string
		args   []string
		action func(context.Context) error
	}{
		{label: "Building core development images", name: makeCommand, args: []string{"images", "COMPONENT=core"}},
		{label: "Building Sandbox Action fixture image", name: makeCommand, args: []string{"images", "COMPONENT=sandbox-action-fixture"}},
		{label: "Loading controller image into kind", name: kindCommand, args: []string{kindLoadSubcommand, kindLoadImageArg, "fast-sandbox/controller:dev", flagName, m.settings.ClusterName}},
		{label: "Loading Fastlet image into kind", name: kindCommand, args: []string{kindLoadSubcommand, kindLoadImageArg, "fast-sandbox/fastlet:dev", flagName, m.settings.ClusterName}},
		{label: "Loading Fastlet Proxy image into kind", name: kindCommand, args: []string{kindLoadSubcommand, kindLoadImageArg, "fast-sandbox/fastlet-proxy:dev", flagName, m.settings.ClusterName}},
		{label: "Loading Sandbox Proxy image into kind", name: kindCommand, args: []string{kindLoadSubcommand, kindLoadImageArg, "fast-sandbox/sandbox-proxy:dev", flagName, m.settings.ClusterName}},
		{label: "Loading NodeJanitor image into kind", name: kindCommand, args: []string{kindLoadSubcommand, kindLoadImageArg, "fast-sandbox/janitor:dev", flagName, m.settings.ClusterName}},
		{label: "Loading Sandbox Action fixture image into kind", name: kindCommand, args: []string{kindLoadSubcommand, kindLoadImageArg, "fast-sandbox/sandbox-action-fixture:dev", flagName, m.settings.ClusterName}},
		{label: "Applying Fast Sandbox namespaces", name: kubectlCommand, args: []string{kubectlApplyVerb, "-k", "config/namespaces"}},
		{label: "Removing legacy default-namespace components", action: m.removeLegacyDefaultComponents},
		{label: "Checking alpha CRD compatibility", action: m.resetIncompatibleAlphaCRDs},
		{label: "Applying CRDs", name: kubectlCommand, args: []string{kubectlApplyVerb, "-k", "config/crd"}},
		{label: "Waiting for Sandbox CRD", name: kubectlCommand, args: []string{"wait", "--for=condition=Established", "crd/sandboxes.sandbox.fast.io", "--timeout=30s"}},
		{label: "Waiting for SandboxPool CRD", name: kubectlCommand, args: []string{"wait", "--for=condition=Established", "crd/sandboxpools.sandbox.fast.io", "--timeout=30s"}},
		{label: "Applying RBAC", name: kubectlCommand, args: []string{kubectlApplyVerb, "-f", "config/rbac/base.yaml"}},
		{label: "Applying runtime environments", name: kubectlCommand, args: []string{kubectlApplyVerb, "-k", "config/runtime-environments"}},
		{label: "Applying development route keys", name: kubectlCommand, args: []string{kubectlApplyVerb, "-f", "config/dev/route-keys.yaml"}},
		{label: "Applying control-plane workloads", name: kubectlCommand, args: []string{kubectlApplyVerb, "-f", "config/manager/controller.yaml"}},
		{label: "Restarting Reconcilers", action: func(ctx context.Context) error {
			return m.rolloutRestart(ctx, "deployment/fast-sandbox-controller")
		}},
		{label: "Waiting for Reconcilers", name: kubectlCommand, args: []string{kubectlRolloutSubcommand, kubectlStatusSubcommand, "deployment/fast-sandbox-controller", "-n", fastSandboxSystemNamespace, kubectlWaitTimeoutFlag}},
		{label: "Restarting Fast-Path servers", action: func(ctx context.Context) error {
			return m.rolloutRestart(ctx, "deployment/fast-sandbox-fastpath")
		}},
		{label: "Waiting for Fast-Path servers", name: kubectlCommand, args: []string{kubectlRolloutSubcommand, kubectlStatusSubcommand, "deployment/fast-sandbox-fastpath", "-n", fastSandboxSystemNamespace, kubectlWaitTimeoutFlag}},
		{label: "Restarting Sandbox Proxy", action: func(ctx context.Context) error {
			return m.rolloutRestart(ctx, "deployment/fast-sandbox-proxy")
		}},
		{label: "Waiting for Sandbox Proxy", name: kubectlCommand, args: []string{kubectlRolloutSubcommand, kubectlStatusSubcommand, "deployment/fast-sandbox-proxy", "-n", fastSandboxSystemNamespace, kubectlWaitTimeoutFlag}},
		{label: "Applying NodeJanitor", name: kubectlCommand, args: []string{kubectlApplyVerb, "-f", "config/janitor/janitor.yaml"}},
		{label: "Restarting NodeJanitor", action: func(ctx context.Context) error {
			return m.rolloutRestart(ctx, "ds/fast-sandbox-janitor")
		}},
		{label: "Waiting for NodeJanitor", name: kubectlCommand, args: []string{kubectlRolloutSubcommand, kubectlStatusSubcommand, "ds/fast-sandbox-janitor", "-n", fastSandboxSystemNamespace, kubectlWaitTimeoutFlag}},
	}

	for index, step := range steps {
		m.progressf("  [%d/%d] %s\n", index+1, len(steps), step.label)
		if step.action != nil {
			if err := step.action(ctx); err != nil {
				return err
			}
			continue
		}
		if _, err := m.runLive(ctx, step.name, step.args...); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) removeLegacyDefaultComponents(ctx context.Context) error {
	resources := []string{
		"deployment/fast-sandbox-controller",
		"deployment/fast-sandbox-fastpath",
		"deployment/fast-sandbox-proxy",
		"daemonset/fast-sandbox-janitor",
		"service/fast-sandbox-fastpath",
		"service/fast-sandbox-proxy",
		"horizontalpodautoscaler/fast-sandbox-fastpath",
		"poddisruptionbudget/fast-sandbox-controller",
		"poddisruptionbudget/fast-sandbox-fastpath",
		"poddisruptionbudget/fast-sandbox-proxy",
	}
	args := append([]string{"delete"}, resources...)
	args = append(args, "-n", "default", "--ignore-not-found=true", "--wait=true")
	_, err := m.runLive(ctx, kubectlCommand, args...)
	return err
}

func (m *Manager) rolloutRestart(ctx context.Context, resource string) error {
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		_, err := m.runLive(
			ctx,
			kubectlCommand, kubectlRolloutSubcommand, "restart", resource, "-n", fastSandboxSystemNamespace,
		)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "restart has already been triggered within the past second") ||
			attempt == attempts {
			return err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// resetIncompatibleAlphaCRDs is intentionally limited to Manager-owned Kind
// environments. v1alpha2 has no conversion webhook and cannot remove
// v1alpha1 from spec.versions while Kubernetes still records v1alpha1 in
// status.storedVersions. The disposable development cluster therefore drops
// only incompatible alpha CRDs before applying the canonical v1alpha2 schema.
func (m *Manager) resetIncompatibleAlphaCRDs(ctx context.Context) error {
	for _, name := range []string{
		"sandboxes.sandbox.fast.io",
		"sandboxpools.sandbox.fast.io",
	} {
		output, err := m.run(ctx, kubectlCommand, "get", "crd", name, "-o", "jsonpath={.status.storedVersions}")
		if err != nil {
			if strings.Contains(err.Error(), "NotFound") {
				continue
			}
			return err
		}
		if !strings.Contains(string(output), "v1alpha1") {
			continue
		}
		m.progressf("    removing incompatible %s (development cluster only)\n", name)
		if _, err := m.runLive(ctx, kubectlCommand, "delete", "crd", name, "--wait=true"); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return m.runCommand(ctx, false, name, args...)
}

func (m *Manager) runLive(ctx context.Context, name string, args ...string) ([]byte, error) {
	return m.runCommand(ctx, true, name, args...)
}

func (m *Manager) runCommand(ctx context.Context, live bool, name string, args ...string) ([]byte, error) {
	var (
		output []byte
		err    error
	)
	if live && m.progress != nil {
		if runner, ok := m.runner.(streamingRunner); ok {
			output, err = runner.RunStreaming(ctx, m.rootDir, m.progress, name, args...)
		} else {
			output, err = m.runner.Run(ctx, m.rootDir, name, args...)
		}
	} else {
		output, err = m.runner.Run(ctx, m.rootDir, name, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w\n%s", commandString(name, args...), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (m *Manager) progressf(format string, args ...any) {
	if m.progress == nil {
		return
	}
	fmt.Fprintf(m.progress, format, args...)
}

func progressDuration(elapsed time.Duration) string {
	if elapsed < 10*time.Millisecond {
		return "<10ms"
	}
	if elapsed < time.Second {
		return elapsed.Round(10 * time.Millisecond).String()
	}
	return elapsed.Round(100 * time.Millisecond).String()
}

type streamingRunner interface {
	RunStreaming(ctx context.Context, dir string, output io.Writer, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func (execRunner) RunStreaming(ctx context.Context, dir string, output io.Writer, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	var captured bytes.Buffer
	stream := io.MultiWriter(output, &captured)
	cmd.Stdout = stream
	cmd.Stderr = stream
	err := cmd.Run()
	return captured.Bytes(), err
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
	return firstNonEmpty(os.Getenv("GVISOR_RELEASE"), "20260714")
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
