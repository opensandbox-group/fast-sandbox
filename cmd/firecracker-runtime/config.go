// config.go — the agent's file-based configuration. All tunables live in
// one YAML file (the fast-sandbox-firecracker-runtime-config ConfigMap,
// mounted at /etc/fast-sandbox/agent-config/agent.yaml); the environment
// carries only the pod-runtime identities injected by the Downward API
// (POD_UID, FAST_SANDBOX_NODE_NAME, FAST_SANDBOX_NODE_IP) and the config
// path itself.
//
// Reload semantics: the socket/state/registry/hostnameFile/p2p sections
// are read once at startup (they wire long-lived resources — changing
// them needs a pod restart); the nodeReadiness section is re-read before
// every recheck pass, so threshold/interval/asset-source edits land
// through the mounted ConfigMap without a restart. Changing
// fcVersion/kernelURL takes effect on the next pass only if the assets
// do not already verify in assetsDir (a pinned install is never silently
// replaced — point assetsDir elsewhere or clear it).
package main

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"

	"fast-sandbox/internal/registryconfig"
	agenthostready "fast-sandbox/internal/runtime/firecracker/agent/hostready"
)

// defaultConfigPath is where the DaemonSet mounts the config ConfigMap.
const defaultConfigPath = "/etc/fast-sandbox/agent-config/agent.yaml"

// defaultHostnameFile is the node hostname file mounted from the host.
const defaultHostnameFile = "/etc/hostname"

// defaultPeerRoutePrefix is the default gateway route prefix (DART's route).
const defaultPeerRoutePrefix = "/dart/"

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
	// P2P configures peer-to-peer artifact distribution; empty = direct
	// header-signed S3 pulls.
	P2P agentP2PConfig `yaml:"p2p"`
	// Dart is the deprecated spelling of p2p.dart (one-release alias,
	// warns at load).
	Dart *agentDartConfig `yaml:"dart,omitempty"`
	// NodeReadiness configures the host-check/asset-install/label loop.
	NodeReadiness agentNodeReadinessConfig `yaml:"nodeReadiness"`
}

// agentP2PConfig selects the peer-distribution provider; dart and gateway
// are mutually exclusive.
type agentP2PConfig struct {
	// Dart starts the node-local DART daemon when Addr is non-empty.
	Dart agentDartConfig `yaml:"dart"`
	// Gateway routes pulls through an external P2P gateway; no child
	// process is started.
	Gateway agentPeerGatewayConfig `yaml:"gateway"`
}

type agentDartConfig struct {
	Addr      string `yaml:"addr"`
	Bin       string `yaml:"bin"`
	Admin     string `yaml:"admin"`
	CacheSize string `yaml:"cacheSize"`
	Discover  string `yaml:"discover"`
	SelfID    string `yaml:"selfID"`
}

type agentPeerGatewayConfig struct {
	// Addr is the gateway base URL (http://host:port); non-empty enables
	// the external mode.
	Addr string `yaml:"addr"`
	// RoutePrefix is the prefix route in front of presigned upstream
	// URLs; empty = "/dart/".
	RoutePrefix string `yaml:"routePrefix"`
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
			if err := config.normalize(); err != nil {
				return config, err
			}
			return config, nil
		}
		return config, fmt.Errorf("read agent config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(payload, &config); err != nil {
		return config, fmt.Errorf("parse agent config %s: %w", path, err)
	}
	if err := config.normalize(); err != nil {
		return config, err
	}
	return config, nil
}

// warnDeprecatedDart keeps the one-release deprecation warning silent on
// the readiness hot-reload path (loadAgentConfig re-runs every pass).
var warnDeprecatedDart sync.Once

// normalize merges the deprecated dart section into p2p, fills defaults
// and validates the provider selection.
func (c *agentConfig) normalize() error {
	if c.Dart != nil {
		warnDeprecatedDart.Do(func() {
			klog.Warningf("the top-level dart config section is deprecated; rename it to p2p.dart (one-release alias)")
		})
		if c.P2P.Dart.Addr == "" {
			c.P2P.Dart = *c.Dart
		}
		c.Dart = nil
	}
	if c.P2P.Dart.Addr != "" && c.P2P.Gateway.Addr != "" {
		return fmt.Errorf("p2p: dart.addr and p2p.gateway.addr are mutually exclusive; configure exactly one peer-distribution provider")
	}
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
	if c.P2P.Dart.Bin == "" {
		c.P2P.Dart.Bin = "dart"
	}
	if c.P2P.Dart.Admin == "" {
		c.P2P.Dart.Admin = "127.0.0.1:8147"
	}
	if c.P2P.Dart.CacheSize == "" {
		c.P2P.Dart.CacheSize = "8GiB"
	}
	if c.P2P.Gateway.Addr != "" && c.P2P.Gateway.RoutePrefix == "" {
		c.P2P.Gateway.RoutePrefix = defaultPeerRoutePrefix
	}
	if c.NodeReadiness.AssetsDir == "" {
		c.NodeReadiness.AssetsDir = agenthostready.DefaultAssetsDir
	}
	if c.NodeReadiness.FCVersion == "" {
		c.NodeReadiness.FCVersion = agenthostready.DefaultFCVersion
	}
	// nodeReadiness.kernelURL stays empty by default: the installer uses
	// the pinned x86_64 Amazon CI kernel (the only supported arch).
	// minFree/minMemory/interval are resolved by the readiness settings
	// loader against the hostready package defaults — no duplicated
	// default literals here.
	return nil
}

// p2pMode reports the effective mode: "dart", "external" or "disabled".
func (c agentConfig) p2pMode() string {
	if c.P2P.Gateway.Addr != "" {
		return "external"
	}
	if c.P2P.Dart.Addr != "" {
		return "dart"
	}
	return "disabled"
}
