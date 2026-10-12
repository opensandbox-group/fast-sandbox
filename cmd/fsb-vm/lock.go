package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

// locksDirName is the directory (under the state or artifact root) holding
// the per-name operation lock files. Its leading dot keeps it out of the
// sandbox id namespace, which must start with an alphanumeric character.
const locksDirName = ".locks"

// errBusy reports that another CLI instance holds the operation lock.
var errBusy = errors.New("operation already in progress")

// tryLock takes a non-blocking exclusive flock on <dir>/.locks/<name>.lock
// and returns its release function. Lock files are never removed: unlinking
// a lock file races with a waiter that already opened it.
func tryLock(dir, name string) (func(), error) {
	lockDir := filepath.Join(dir, locksDirName)
	if err := os.MkdirAll(lockDir, 0o750); err != nil {
		return nil, fmt.Errorf("create lock directory %s: %w", lockDir, err)
	}
	lockPath := filepath.Join(lockDir, name+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errBusy
		}
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// lockSandbox serializes create and delete of one sandbox id.
func lockSandbox(sandboxID string) (func(), error) {
	release, err := tryLock(global.stateDir, sandboxID)
	if errors.Is(err, errBusy) {
		return nil, fmt.Errorf("another create/delete of sandbox %q is in progress", sandboxID)
	}
	return release, err
}

// sandboxIDPattern admits a single safe directory name: it rejects ".", "..",
// path separators and hidden names, so <state-dir>/<id> can never escape or
// alias the state root.
var sandboxIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateSandboxID enforces sandboxIDPattern; create, delete and list share
// it.
func validateSandboxID(id string) error {
	if !sandboxIDPattern.MatchString(id) {
		return fmt.Errorf("invalid sandbox_id %q: want 1-64 characters of [A-Za-z0-9._-] starting with a letter or digit", id)
	}
	return nil
}
