package podcgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

const cpuPeriodMicros int64 = 100_000

// RuntimeBudget is the aggregate physical budget available to Sandbox
// runtimes below one Fastlet Pod. Fastlet overhead is deliberately excluded.
type RuntimeBudget struct {
	CPU    resource.Quantity
	Memory resource.Quantity
}

// SandboxLimits are the host cgroup limits of one Sandbox runtime process.
// Their sum may exceed RuntimeBudget; the bounded parent is the overcommit
// correctness boundary.
type SandboxLimits struct {
	CPU        resource.Quantity
	CPURequest resource.Quantity
	PIDs       int64
}

// RuntimeManager owns the cgroup v2 subtree used by Sandbox runtime
// processes. The Fastlet itself remains in its Kubernetes container cgroup,
// while this subtree is a sibling below the Kubernetes Pod cgroup. This keeps
// Sandbox-budget OOM victim selection away from the Fastlet without turning
// the container cgroup into an internal node (which would break later execs).
type RuntimeManager struct {
	root   string
	layout Layout
	budget RuntimeBudget

	mu       sync.Mutex
	prepared bool
	leaves   map[string]string
}

// NewRuntimeManager discovers the current Fastlet Pod and prepares a manager
// for its deterministic fast-sandbox subtree. Prepare must be called before
// a runtime leaf is requested.
func NewRuntimeManager(root, podUID string, budget RuntimeBudget) (*RuntimeManager, error) {
	if budget.CPU.Sign() <= 0 || budget.Memory.Sign() <= 0 {
		return nil, errors.New("Sandbox runtime CPU and memory budget must be greater than zero")
	}
	layout, err := Discover(root, podUID)
	if err != nil {
		return nil, fmt.Errorf("discover Fastlet Pod cgroup: %w", err)
	}
	if layout.Version != VersionV2 {
		return nil, fmt.Errorf("bounded Sandbox runtime hierarchy requires cgroup v2, found %s", layout.Version)
	}
	return &RuntimeManager{root: root, layout: layout, budget: budget, leaves: make(map[string]string)}, nil
}

// Prepare creates and bounds the shared Sandbox subtree. Enabling controllers
// at the Pod parent is safe because Kubernetes places containers in children;
// the Pod cgroup itself must not contain processes.
func (m *RuntimeManager) Prepare() error {
	if m == nil {
		return errors.New("runtime cgroup manager is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	podPath := cgroupFilesystemPath(m.root, m.layout.PodPath)
	if err := enableV2Controllers(podPath); err != nil {
		return fmt.Errorf("enable Pod cgroup controllers: %w", err)
	}
	aggregatePath := m.AggregateFilesystemPath()
	if err := os.MkdirAll(aggregatePath, 0o755); err != nil {
		return fmt.Errorf("create Sandbox runtime cgroup %q: %w", aggregatePath, err)
	}
	if err := writeCPUQuota(aggregatePath, m.budget.CPU); err != nil {
		return fmt.Errorf("apply aggregate Sandbox CPU budget: %w", err)
	}
	if err := writeMemoryLimit(aggregatePath, m.budget.Memory); err != nil {
		return fmt.Errorf("apply aggregate Sandbox memory budget: %w", err)
	}
	if err := disableSwap(aggregatePath, "/proc/swaps"); err != nil {
		return fmt.Errorf("disable aggregate Sandbox swap: %w", err)
	}
	if err := writeOptionalControl(aggregatePath, "memory.oom.group", "0"); err != nil {
		return fmt.Errorf("configure aggregate Sandbox OOM policy: %w", err)
	}
	if err := enableV2Controllers(aggregatePath); err != nil {
		return fmt.Errorf("enable Sandbox leaf controllers: %w", err)
	}
	m.prepared = true
	return nil
}

// AggregatePath is the host-visible cgroup path below the cgroup v2 mount.
func (m *RuntimeManager) AggregatePath() string {
	if m == nil {
		return ""
	}
	return filepath.ToSlash(filepath.Join(m.layout.PodPath, "fast-sandbox"))
}

// AggregateFilesystemPath is the path through the Fastlet's writable host
// cgroup mount. It is also the memory observer target.
func (m *RuntimeManager) AggregateFilesystemPath() string {
	if m == nil {
		return ""
	}
	return cgroupFilesystemPath(m.root, m.AggregatePath())
}

// EnsureSandbox creates a leaf and applies the resolved host limits. The leaf
// memory OOM policy groups the VMM process and any descendants as one victim,
// while an aggregate-parent OOM may select one Sandbox leaf without selecting
// the Fastlet process in its sibling container cgroup.
func (m *RuntimeManager) EnsureSandbox(sandboxID string, limits SandboxLimits) (string, error) {
	defer observeSetup(time.Now())
	if m == nil {
		return "", errors.New("runtime cgroup manager is required")
	}
	if sandboxID == "" {
		return "", errors.New("sandbox ID is required")
	}
	if limits.CPU.Sign() <= 0 || limits.CPURequest.Sign() <= 0 || limits.CPURequest.Cmp(limits.CPU) > 0 || limits.PIDs <= 0 {
		return "", errors.New("invalid Sandbox CPU request, CPU limit, or PIDs limit")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.prepared {
		return "", errors.New("runtime cgroup hierarchy is not prepared")
	}

	cgroupPath, err := m.layout.SandboxPath(sandboxID)
	if err != nil {
		return "", err
	}
	path := cgroupFilesystemPath(m.root, cgroupPath)
	err = os.Mkdir(path, 0o755)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("create Sandbox cgroup %q: %w", path, err)
	}
	created := err == nil
	success := false
	defer func() {
		if created && !success {
			_ = os.Remove(path)
		}
	}()
	if err := writeCPUQuota(path, limits.CPU); err != nil {
		return "", fmt.Errorf("apply Sandbox CPU limit: %w", err)
	}
	if err := writeControl(path, "cpu.weight", strconv.FormatInt(cpuWeight(limits.CPURequest), 10)); err != nil {
		return "", fmt.Errorf("apply Sandbox CPU weight: %w", err)
	}
	if err := writeControl(path, "memory.max", "max"); err != nil {
		return "", fmt.Errorf("set shared Sandbox memory policy: %w", err)
	}
	if err := writeControl(path, "pids.max", strconv.FormatInt(limits.PIDs, 10)); err != nil {
		return "", fmt.Errorf("apply Sandbox PIDs limit: %w", err)
	}
	if err := writeOptionalControl(path, "memory.oom.group", "1"); err != nil {
		return "", fmt.Errorf("configure Sandbox OOM group: %w", err)
	}
	success = true
	m.leaves[sandboxID] = path
	return path, nil
}

// Map Kubernetes CPU shares [2, 262144] onto cgroup v2 weights [1, 10000].
func cpuWeight(request resource.Quantity) int64 {
	shares := max(int64(2), min(request.MilliValue(), int64(256000))*1024/1000)
	return 1 + (shares-2)*9999/262142
}

func disableSwap(path, swapsFile string) error {
	control := filepath.Join(path, "memory.swap.max")
	if _, err := os.Stat(control); err == nil {
		return writeControl(path, "memory.swap.max", "0")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	swaps, err := os.ReadFile(swapsFile)
	if err != nil {
		return err
	}
	header, devices, _ := strings.Cut(strings.TrimSpace(string(swaps)), "\n")
	if !strings.HasPrefix(header, "Filename") || strings.TrimSpace(devices) != "" {
		return errors.New("swap is available without memory.swap.max")
	}
	return nil
}

// Empty reports a confirmed exit only. Missing or unreadable observations
// are never evidence for terminating a Sandbox.
func (m *RuntimeManager) Empty(id string) bool {
	m.mu.Lock()
	path := m.leaves[id]
	m.mu.Unlock()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	return err == nil && strings.Contains("\n"+string(data)+"\n", "\npopulated 0\n")
}

// ConfirmAbsent checks kernel state after a restart, before any leaf has
// been registered in memory. A dead VM's old PID may already have been reused.
func (m *RuntimeManager) ConfirmAbsent(sandboxID string) error {
	cgroupPath, err := m.layout.SandboxPath(sandboxID)
	if err != nil {
		return err
	}
	path := cgroupFilesystemPath(m.root, cgroupPath)
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.Contains("\n"+string(data)+"\n", "\npopulated 0\n") {
		return fmt.Errorf("Sandbox cgroup %q still contains processes", path)
	}
	return nil
}

// SandboxProcesses returns the processes in the deterministic Sandbox leaf.
// A missing leaf is distinguished from an unreadable leaf so recovery never
// treats an unknown ownership observation as proof of absence.
func (m *RuntimeManager) SandboxProcesses(sandboxID string) ([]int, bool, error) {
	if m == nil {
		return nil, false, errors.New("runtime cgroup manager is required")
	}
	cgroupPath, err := m.layout.SandboxPath(sandboxID)
	if err != nil {
		return nil, false, err
	}
	path := cgroupFilesystemPath(m.root, cgroupPath)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	raw, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return nil, true, fmt.Errorf("read Sandbox cgroup processes %q: %w", path, err)
	}
	fields := strings.Fields(string(raw))
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			return nil, true, fmt.Errorf("parse Sandbox cgroup PID %q in %q", field, path)
		}
		pids = append(pids, pid)
	}
	return pids, true, nil
}

// RemoveSandbox removes an empty leaf after its runtime process exits.
func (m *RuntimeManager) RemoveSandbox(sandboxID string) error {
	if m == nil || sandboxID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cgroupPath, err := m.layout.SandboxPath(sandboxID)
	if err != nil {
		return err
	}
	path := cgroupFilesystemPath(m.root, cgroupPath)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove Sandbox runtime cgroup %q: %w", path, err)
	}
	delete(m.leaves, sandboxID)
	return nil
}

func enableV2Controllers(path string) error {
	controllers, err := os.ReadFile(filepath.Join(path, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read cgroup.controllers: %w", err)
	}
	available := strings.Fields(string(controllers))
	required := []string{"cpu", "memory", "pids"}
	for _, controller := range required {
		if !containsString(available, controller) {
			return fmt.Errorf("required controller %q is unavailable in %q", controller, path)
		}
	}
	current, err := os.ReadFile(filepath.Join(path, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("read cgroup.subtree_control: %w", err)
	}
	enabled := strings.Fields(string(current))
	missing := make([]string, 0, len(required))
	for _, controller := range required {
		if !containsString(enabled, controller) {
			missing = append(missing, "+"+controller)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	if err := os.WriteFile(filepath.Join(path, "cgroup.subtree_control"), []byte(strings.Join(missing, " ")), 0o600); err != nil {
		return fmt.Errorf("write cgroup.subtree_control: %w", err)
	}
	return nil
}

func writeCPUQuota(path string, quantity resource.Quantity) error {
	milli := quantity.MilliValue()
	if milli <= 0 {
		return errors.New("CPU quantity must be greater than zero")
	}
	quota := (milli*cpuPeriodMicros + 999) / 1000
	// cgroup v2 rejects a quota below one millisecond.
	if quota < 1000 {
		quota = 1000
	}
	return writeControl(path, "cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriodMicros))
}

func writeMemoryLimit(path string, quantity resource.Quantity) error {
	value := quantity.Value()
	if value <= 0 {
		return errors.New("memory quantity must be greater than zero")
	}
	return writeControl(path, "memory.max", strconv.FormatInt(value, 10))
}

func writeControl(path, name, value string) error {
	if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func writeOptionalControl(path, name, value string) error {
	control := filepath.Join(path, name)
	if _, err := os.Stat(control); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeControl(path, name, value)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
