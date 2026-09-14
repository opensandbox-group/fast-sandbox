// Package artifactstore resolves the platform artifact store address shared
// by the controller, the node runtime-agent and golden-image builds.
//
// The value lives in the fast-sandbox-artifact-store ConfigMap, projected by
// kubelet into DefaultMountDir (one file per key). There is no flag or env
// override: callers read the mounted files at use time (per reconcile, per
// pull) instead of caching them, so a ConfigMap edit reaches every component
// without a restart — kubelet swaps the projected files in place and the next
// read observes the new revision.
package artifactstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultMountDir is where deployments project the shared
	// fast-sandbox-artifact-store ConfigMap. The zero Loader reads it.
	DefaultMountDir = "/etc/fast-sandbox/artifact-store"

	// StoreKey and EndpointKey are the ConfigMap keys, each projected as a
	// file of the same name.
	StoreKey    = "store"
	EndpointKey = "endpoint"
)

// Config is a resolved artifact store address.
type Config struct {
	// Store is the store root, s3://bucket/prefix.
	Store string
	// Endpoint is the optional S3-compatible endpoint
	// (scheme://host:port); empty derives it from the credential.
	Endpoint string
}

// Loader reads the projected ConfigMap on every Load. The zero value reads
// DefaultMountDir; Dir overrides it for tests and non-Kubernetes runs.
type Loader struct {
	Dir string
}

// Load reads the mounted configuration. Missing mount files resolve to empty
// values (a cluster without the ConfigMap is unconfigured, not broken);
// unreadable files are an error.
func (l Loader) Load() (Config, error) {
	dir := l.Dir
	if dir == "" {
		dir = DefaultMountDir
	}
	return loadDir(dir)
}

func loadDir(dir string) (Config, error) {
	store, err := readKey(dir, StoreKey)
	if err != nil {
		return Config{}, err
	}
	endpoint, err := readKey(dir, EndpointKey)
	if err != nil {
		return Config{}, err
	}
	return Config{Store: store, Endpoint: endpoint}, nil
}

func readKey(dir, key string) (string, error) {
	payload, err := os.ReadFile(filepath.Join(dir, key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read artifact store %s from %s: %w", key, dir, err)
	}
	return strings.TrimSpace(string(payload)), nil
}
