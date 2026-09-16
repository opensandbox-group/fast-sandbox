// Package main — config.go: the agent's file-based configuration. All
// tunables live in one YAML file (the fast-sandbox-firecracker-runtime-
// config ConfigMap, mounted at /etc/fast-sandbox/agent-config/agent.yaml);
// the environment carries only the pod-runtime identities injected by the
// Downward API (POD_UID, FAST_SANDBOX_NODE_NAME, FAST_SANDBOX_NODE_IP) and
// the config path itself.
//
// Reload semantics: the socket/state/registry/dart sections are read once
// at startup (they wire long-lived resources — changing them needs a pod
// restart); the nodeReadiness section is re-read before every recheck pass,
// so threshold/interval/asset-source edits land through the mounted
// ConfigMap without a restart. Changing fcVersion/kernelURL takes effect
// on the next pass only if the assets do not already verify in
// assetsDir (a pinned install is never silently replaced — point
// assetsDir elsewhere or clear it).
package main

import (
	"fmt"
	"os"

	"fast-sandbox/internal/registryconfig"
	agenthostready "fast-sandbox/internal/runtime/firecracker/agent/hostready"

	"gopkg.in/yaml.v3"
)

// defaultConfigPath is where the DaemonSet mounts the config ConfigMap.
const defaultConfigPath = "/etc/fast-sandbox/agent-config/agent.yaml"

// defaultHostnameFile is the node hostname file mounted from the host.
const defaultHostnameFile = "/etc/hostname"

// agentConfig is the full agent configuration file schema. Every field may
// be omitted to take the default.
type agentConfig struct {
	// Socket is the UDS socket the management API serves on.
	Socket string `yaml:"socket"`
	// StateRoot is the shared artifact cache / lease state root.
	StateRoot string `yaml:"stateRoot"`
	// RegistryConfig is the compiled registry credential file.
	RegistryConfig string `yaml:"registryConfig"`
	// HostnameFile is the node hostname file (the DaemonSet mounts the
	// node's /etc/hostname; a pod's own hostname is its pod name).
	HostnameFile string `yaml:"hostnameFile"`
	// Dart configures the node-local DART P2P daemon. An empty Addr
	// disables the gateway (pulls stay on the direct S3 path).
	Dart agentDartConfig `yaml:"dart"`
	// NodeReadiness configures the host-check/asset-install/label loop.
	NodeReadiness agentNodeReadinessConfig `yaml:"nodeReadiness"`
}

type agentDartConfig struct {
	Addr      string `yaml:"addr"`
	Bin       string `yaml:"bin"`
	Admin     string `yaml:"admin"`
	CacheSize string `yaml:"cacheSize"`
	Discover  string `yaml:"discover"`
	SelfID    string `yaml:"selfID"`
}

type agentNodeReadinessConfig struct {
	// Enabled turns the readiness loop on (the DaemonSet sets this plus
	// FAST_SANDBOX_NODE_NAME).
	Enabled bool `yaml:"enabled"`
	// AssetsDir/FCVersion/KernelURL feed the Firecracker asset install.
	AssetsDir string `yaml:"assetsDir"`
	FCVersion string `yaml:"fcVersion"`
	KernelURL string `yaml:"kernelURL"`
	// Interval is the recheck cadence ("5m").
	Interval string `yaml:"interval"`
	// MinFree/MinMemory are the byte thresholds ("10GiB").
	MinFree   string `yaml:"minFree"`
	MinMemory string `yaml:"minMemory"`
}

// loadAgentConfig reads the config file and fills every omitted field with
// its default. A missing file yields the defaults (bare-process runs like
// the chain E2E start without a config); a present-but-invalid file is an
// error (a typo must not silently configure the node wrong).
func loadAgentConfig(path string) (agentConfig, error) {
	config := agentConfig{}
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			config.fillDefaults()
			return config, nil
		}
		return config, fmt.Errorf("read agent config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(payload, &config); err != nil {
		return config, fmt.Errorf("parse agent config %s: %w", path, err)
	}
	config.fillDefaults()
	return config, nil
}

// fillDefaults applies the built-in defaults to every empty field.
func (c *agentConfig) fillDefaults() {
	if c.Socket == "" {
		c.Socket = defaultSocketPath
	}
	if c.StateRoot == "" {
		c.StateRoot = defaultStateRoot
	}
	if c.RegistryConfig == "" {
		c.RegistryConfig = registryconfig.MountPath
	}
	if c.HostnameFile == "" {
		c.HostnameFile = defaultHostnameFile
	}
	if c.Dart.Bin == "" {
		c.Dart.Bin = "dart"
	}
	if c.Dart.Admin == "" {
		c.Dart.Admin = "127.0.0.1:8147"
	}
	if c.Dart.CacheSize == "" {
		c.Dart.CacheSize = "8GiB"
	}
	if c.NodeReadiness.AssetsDir == "" {
		c.NodeReadiness.AssetsDir = agenthostready.DefaultAssetsDir
	}
	if c.NodeReadiness.FCVersion == "" {
		c.NodeReadiness.FCVersion = agenthostready.DefaultFCVersion
	}
	if c.NodeReadiness.KernelURL == "" {
		c.NodeReadiness.KernelURL = agenthostready.DefaultKernelURL
	}
	if c.NodeReadiness.Interval == "" {
		c.NodeReadiness.Interval = "5m"
	}
	if c.NodeReadiness.MinFree == "" {
		c.NodeReadiness.MinFree = "10GiB"
	}
	if c.NodeReadiness.MinMemory == "" {
		c.NodeReadiness.MinMemory = "2GiB"
	}
}
