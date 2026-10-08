package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// mergeCredentials merges the entries of the template's dockerAuth into
// overlaybd's global credential file (default /opt/overlaybd/cred.json).
// overlaybd lazily reloads this file per remote_path, so without the merge
// on-demand block reads fail with 401 (ADR-003). The merge is serialized with
// an flock on a sidecar lock file and committed atomically (tmp+rename) so
// concurrent CLI instances cannot clobber each other.
func mergeCredentials(credFile string, incoming dockerConfig) error {
	if len(incoming.Auths) == 0 {
		return nil
	}
	directory := filepath.Dir(credFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create credential directory %s: %w", directory, err)
	}

	unlock, err := lockCredentials(credFile)
	if err != nil {
		return err
	}
	defer unlock()

	existing, err := readCredentials(credFile)
	if err != nil {
		return err
	}
	if existing.Auths == nil {
		existing.Auths = map[string]authEntry{}
	}
	for host, entry := range incoming.Auths {
		existing.Auths[host] = entry
	}
	return writeCredentialsAtomic(credFile, existing)
}

// lockCredentials acquires an exclusive advisory lock guarding credFile and
// returns a release function.
func lockCredentials(credFile string) (func(), error) {
	lockPath := credFile + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open credential lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock credential file: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// readCredentials loads the existing cred.json; a missing file is an empty
// config.
func readCredentials(credFile string) (dockerConfig, error) {
	payload, err := os.ReadFile(credFile)
	if err != nil {
		if os.IsNotExist(err) {
			return dockerConfig{Auths: map[string]authEntry{}}, nil
		}
		return dockerConfig{}, fmt.Errorf("read credential file %s: %w", credFile, err)
	}
	if len(payload) == 0 {
		return dockerConfig{Auths: map[string]authEntry{}}, nil
	}
	var config dockerConfig
	if err := json.Unmarshal(payload, &config); err != nil {
		return dockerConfig{}, fmt.Errorf("parse credential file %s: %w", credFile, err)
	}
	return config, nil
}

// writeCredentialsAtomic serializes config and installs it with tmp+rename.
func writeCredentialsAtomic(credFile string, config dockerConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(credFile), ".cred-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp credential file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temp credential file: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temp credential file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temp credential file: %w", err)
	}
	if err := os.Rename(tempName, credFile); err != nil {
		return fmt.Errorf("commit credential file %s: %w", credFile, err)
	}
	return nil
}
