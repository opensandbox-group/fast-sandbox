package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	infracatalog "fast-sandbox/internal/catalog/infra"
)

const defaultStopGrace = 5 * time.Second

type Supervisor struct {
	Stdout io.Writer
	Stderr io.Writer

	mu        sync.Mutex
	processes map[int]*os.Process
	stopping  bool
}

func NewSupervisor(stdout, stderr io.Writer) *Supervisor {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &Supervisor{Stdout: stdout, Stderr: stderr, processes: make(map[int]*os.Process)}
}

// Run starts Infra Components and the original image process. Infra starts in
// parallel with the user process by default; only StartBeforeUser components
// gate user startup. The returned code is always the user process exit code.
func (s *Supervisor) Run(ctx context.Context, config Config, userArgs []string) (int, error) {
	if err := config.Validate(); err != nil {
		return 1, err
	}
	if len(userArgs) == 0 || userArgs[0] == "" {
		return 1, errors.New("original user entrypoint is empty")
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	components, err := orderedComponents(config.Components)
	if err != nil {
		return 1, err
	}
	for _, component := range components {
		if !component.StartBeforeUser {
			continue
		}
		if err := s.startComponent(runContext, component); err != nil {
			if !component.Required {
				fmt.Fprintf(s.Stderr, "sandbox-init: optional pre-user component %s failed to start: %v\n", component.Name, err)
				continue
			}
			s.stopAll(syscall.SIGTERM, defaultStopGrace)
			return 1, fmt.Errorf("start required pre-user component %s: %w", component.Name, err)
		}
		if err := waitReady(runContext, component.Readiness); err != nil {
			if !component.Required {
				fmt.Fprintf(s.Stderr, "sandbox-init: optional pre-user component %s readiness failed: %v\n", component.Name, err)
				continue
			}
			s.stopAll(syscall.SIGTERM, defaultStopGrace)
			return 1, fmt.Errorf("pre-user component %s readiness: %w", component.Name, err)
		}
	}
	for _, component := range components {
		if component.StartBeforeUser {
			continue
		}
		if err := s.startComponent(runContext, component); err != nil {
			if !component.Required {
				fmt.Fprintf(s.Stderr, "sandbox-init: optional component %s failed to start: %v\n", component.Name, err)
				continue
			}
			s.stopAll(syscall.SIGTERM, defaultStopGrace)
			return 1, fmt.Errorf("start component %s: %w", component.Name, err)
		}
	}
	user := exec.Command(userArgs[0], userArgs[1:]...)
	user.Stdout, user.Stderr, user.Stdin = s.Stdout, s.Stderr, os.Stdin
	user.SysProcAttr = userProcessAttributes(config.UserCredential)
	if err := user.Start(); err != nil {
		s.stopAll(syscall.SIGTERM, defaultStopGrace)
		return 1, fmt.Errorf("start user process: %w", err)
	}
	s.track(user.Process)
	userDone := make(chan error, 1)
	go func() {
		err := user.Wait()
		s.untrack(user.Process.Pid)
		userDone <- err
	}()

	select {
	case err := <-userDone:
		cancel()
		s.stopAll(syscall.SIGTERM, defaultStopGrace)
		return exitCode(err), nil
	case <-ctx.Done():
		s.Forward(syscall.SIGTERM)
		s.stopAll(syscall.SIGTERM, defaultStopGrace)
		err := <-userDone
		return exitCode(err), ctx.Err()
	}
}

func userProcessAttributes(credential *UserCredential) *syscall.SysProcAttr {
	attributes := &syscall.SysProcAttr{Setpgid: true}
	if credential == nil {
		return attributes
	}
	attributes.Credential = &syscall.Credential{
		Uid: credential.UID, Gid: credential.GID,
		Groups: append([]uint32(nil), credential.AdditionalGIDs...),
	}
	return attributes
}

func (s *Supervisor) startComponent(ctx context.Context, component Component) error {
	command := exec.Command(component.Command, component.Args...)
	command.Stdout, command.Stderr = s.Stdout, s.Stderr
	command.Env = mergeEnv(os.Environ(), component.Env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return err
	}
	s.track(command.Process)
	go s.monitorComponent(ctx, component, command)
	return nil
}

func (s *Supervisor) monitorComponent(ctx context.Context, component Component, command *exec.Cmd) {
	for {
		err := command.Wait()
		s.untrack(command.Process.Pid)
		fmt.Fprintf(s.Stderr, "sandbox-init: component %s exited with code %d\n", component.Name, exitCode(err))
		if ctx.Err() != nil || !shouldRestart(component.RestartPolicy, err) {
			return
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		command = exec.Command(component.Command, component.Args...)
		command.Stdout, command.Stderr = s.Stdout, s.Stderr
		command.Env = mergeEnv(os.Environ(), component.Env)
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := command.Start(); err != nil {
			fmt.Fprintf(s.Stderr, "sandbox-init: component %s restart failed: %v\n", component.Name, err)
			return
		}
		fmt.Fprintf(s.Stderr, "sandbox-init: component %s restarted\n", component.Name)
		s.track(command.Process)
	}
}

func orderedComponents(components []Component) ([]Component, error) {
	ordered := make([]Component, 0, len(components))
	added := make(map[string]struct{}, len(components))
	for len(ordered) < len(components) {
		progress := false
		for _, component := range components {
			if _, exists := added[component.Name]; exists {
				continue
			}
			ready := true
			for _, dependency := range component.DependsOn {
				if _, exists := added[dependency]; !exists {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			ordered = append(ordered, component)
			added[component.Name] = struct{}{}
			progress = true
		}
		if !progress {
			return nil, errors.New("component dependency graph contains a cycle")
		}
	}
	return ordered, nil
}

func (s *Supervisor) Forward(signal os.Signal) {
	syscallSignal, ok := signal.(syscall.Signal)
	if !ok {
		return
	}
	s.mu.Lock()
	processes := make([]*os.Process, 0, len(s.processes))
	for _, process := range s.processes {
		processes = append(processes, process)
	}
	s.mu.Unlock()
	for _, process := range processes {
		_ = syscall.Kill(-process.Pid, syscallSignal)
	}
}

func (s *Supervisor) stopAll(signal syscall.Signal, grace time.Duration) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	s.mu.Unlock()
	s.Forward(signal)
	deadline := time.NewTimer(grace)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		s.mu.Lock()
		remaining := len(s.processes)
		s.mu.Unlock()
		if remaining == 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			s.Forward(syscall.SIGKILL)
			return
		}
	}
}

func (s *Supervisor) track(process *os.Process) {
	if process == nil {
		return
	}
	s.mu.Lock()
	s.processes[process.Pid] = process
	s.mu.Unlock()
}

func (s *Supervisor) untrack(pid int) {
	s.mu.Lock()
	delete(s.processes, pid)
	s.mu.Unlock()
}

func waitReady(ctx context.Context, readiness Readiness) error {
	if readiness.Type == "" || readiness.Type == infracatalog.ProbeNone {
		return nil
	}
	timeout := readiness.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	retryCeiling := readiness.Interval
	if retryCeiling <= 0 || retryCeiling > 10*time.Millisecond {
		retryCeiling = 10 * time.Millisecond
	}
	retryDelay := min(time.Millisecond, retryCeiling)
	probeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var err error
		switch readiness.Type {
		case infracatalog.ProbeHTTP:
			request, requestErr := http.NewRequestWithContext(probeContext, http.MethodGet, "http://"+readiness.Address+readiness.Path, nil)
			if requestErr != nil {
				return requestErr
			}
			response, requestErr := http.DefaultClient.Do(request)
			err = requestErr
			if response != nil {
				_ = response.Body.Close()
				if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
					err = fmt.Errorf("readiness returned HTTP %d", response.StatusCode)
				}
			}
		case infracatalog.ProbeTCP:
			var connection net.Conn
			connection, err = (&net.Dialer{}).DialContext(probeContext, "tcp", readiness.Address)
			if connection != nil {
				_ = connection.Close()
			}
		default:
			return fmt.Errorf("unsupported readiness type %s", readiness.Type)
		}
		if err == nil {
			return nil
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-probeContext.Done():
			timer.Stop()
			return errors.Join(probeContext.Err(), err)
		case <-timer.C:
		}
		retryDelay = min(retryDelay*2, retryCeiling)
	}
}

func shouldRestart(policy infracatalog.RestartPolicy, err error) bool {
	switch policy {
	case infracatalog.RestartAlways:
		return true
	case infracatalog.RestartOnFailure:
		return exitCode(err) != 0
	default:
		return false
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return 1
}

func mergeEnv(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
