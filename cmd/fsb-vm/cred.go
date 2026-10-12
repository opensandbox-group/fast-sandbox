package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// overlaybdCredentials re-keys the template credential for overlaybd. ORAS
// matches docker config keys loosely (scheme, Docker Hub aliases, /v1/
// suffixes), but overlaybd's parse_auths only accepts a key exactly equal to
// the blob URL's host or a host/repository prefix of it. For every image the
// sandbox reads, the credential ORAS would use is therefore written under
// <canonical host>/<repository>, the exact prefix of that image's
// repoBlobUrl.
func overlaybdCredentials(config dockerConfig, imageRefs ...string) (dockerConfig, error) {
	handoff := dockerConfig{Auths: map[string]authEntry{}}
	for _, imageRef := range imageRefs {
		ref, err := parseRegistryReference(imageRef)
		if err != nil {
			return dockerConfig{}, err
		}
		credential, found, err := credentialFor(config, ref)
		if err != nil {
			return dockerConfig{}, fmt.Errorf("credential for %s: %w", imageRef, err)
		}
		if !found || (credential.Username == "" && credential.Password == "") {
			continue
		}
		key := canonicalRegistryHost(ref.Registry) + "/" + ref.Repository
		handoff.Auths[key] = authEntry{
			Auth: base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password)),
		}
	}
	return handoff, nil
}

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

// writeCredentialsAtomic serializes config and installs it with
// tmp+rename+fsync.
func writeCredentialsAtomic(credFile string, config dockerConfig) error {
	data, err := marshalCredentials(config)
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
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temp credential file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temp credential file: %w", err)
	}
	if err := os.Rename(tempName, credFile); err != nil {
		return fmt.Errorf("commit credential file %s: %w", credFile, err)
	}
	return nil
}

// marshalCredentials renders cred.json with the auths keys ordered longest
// first. overlaybd walks the keys in file order and stops at the first one
// matching the blob URL, so this ordering makes its first match the most
// specific (longest-prefix) one, as with ORAS.
func marshalCredentials(config dockerConfig) ([]byte, error) {
	keys := make([]string, 0, len(config.Auths))
	for key := range config.Auths {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	var buffer bytes.Buffer
	buffer.WriteString("{\n  \"auths\": {")
	for index, key := range keys {
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		entry, err := json.MarshalIndent(config.Auths[key], "    ", "  ")
		if err != nil {
			return nil, err
		}
		if index > 0 {
			buffer.WriteByte(',')
		}
		buffer.WriteString("\n    ")
		buffer.Write(name)
		buffer.WriteString(": ")
		buffer.Write(entry)
	}
	if len(keys) > 0 {
		buffer.WriteString("\n  ")
	}
	buffer.WriteString("}\n}\n")
	return buffer.Bytes(), nil
}
