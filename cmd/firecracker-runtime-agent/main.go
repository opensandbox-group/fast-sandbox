package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"fast-sandbox/internal/artifactstore"
	"fast-sandbox/internal/registryconfig"
	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
	agentdart "fast-sandbox/internal/runtime/firecracker/agent/dart"
	agentserver "fast-sandbox/internal/runtime/firecracker/agent/server"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"

	"k8s.io/klog/v2"
)

const (
	// defaultSocketPath is the UDS socket served by this agent.
	defaultSocketPath = "/run/fast-sandbox/firecracker/runtime.sock"
	// defaultStateRoot mirrors the fastlet driver's StateRoot, so the
	// shared cache (images/) and the driver's resolveRootfsImage agree.
	defaultStateRoot = "/var/lib/fast-sandbox/firecracker"
)

func main() {
	if err := run(); err != nil {
		klog.ErrorS(err, "firecracker-runtime-agent failed")
		os.Exit(1)
	}
}

// run assembles the agent. It returns an error when the agent cannot serve;
// deferred cleanup (lease state close, DART child stop) runs on the way out.
func run() error {
	socketPath := getEnv("FAST_SANDBOX_RUNTIME_AGENT_SOCKET", defaultSocketPath)
	stateRoot := getEnv("FAST_SANDBOX_STATE_ROOT", defaultStateRoot)
	registryPath := getEnv("FAST_SANDBOX_REGISTRY_CONFIG_PATH", registryconfig.MountPath)
	// The artifact store is read from the mounted fast-sandbox-artifact-store
	// ConfigMap on every pull, so a ConfigMap edit applies to the next pull
	// without an agent restart.
	storeConfig := artifactstore.Loader{}

	registryProvider := registryconfig.NewFileProvider(registryPath)

	// DART P2P gateway (stage 2). FAST_SANDBOX_DART_ADDR empty = local
	// mode: artifact pulls stay on the direct header-signed S3 path.
	// Non-empty = the node-local DART daemon is orchestrated as a child
	// process and artifact bytes route through its prefix API as presigned
	// URLs (with direct-S3 fallback when DART is unreachable).
	dartAddr := getEnv("FAST_SANDBOX_DART_ADDR", "")
	var pullOptions []agentpull.Option
	var dartManager *agentdart.Manager
	if dartAddr != "" {
		listen, err := dartListenAddress(dartAddr)
		if err != nil {
			return err
		}
		// A stable identity anchors the HRW keyspace AND names the per-node
		// block cache: the StateRoot can be shared (multi-node kind mounts
		// one host filesystem into every node container), but two DART
		// arenas must never point at the same directory. The node's
		// hostname is read from /etc/hostname (mounted from the node by the
		// DaemonSet) because a regular pod's own hostname is its pod name,
		// which changes on restart.
		nodeID := getEnv("FAST_SANDBOX_DART_SELF_ID", "")
		if nodeID == "" {
			nodeID = nodeHostID()
		}
		peerPort := "9000"
		config := agentdart.Config{
			Binary:    getEnv("FAST_SANDBOX_DART_BIN", "dart"),
			Listen:    listen,
			Admin:     getEnv("FAST_SANDBOX_DART_ADMIN", "127.0.0.1:8147"),
			CacheDir:  filepath.Join(stateRoot, "cache", "dart-"+strings.ReplaceAll(nodeID, "/", "-")),
			CacheSize: getEnv("FAST_SANDBOX_DART_CACHE_SIZE", "8GiB"),
			Discover:  getEnv("FAST_SANDBOX_DART_DISCOVER", ""),
			SelfID:    nodeID,
			Log:       os.Stderr,
		}
		if nodeIP := getEnv("FAST_SANDBOX_NODE_IP", ""); nodeIP != "" {
			config.PeerAdvertise = net.JoinHostPort(nodeIP, peerPort)
		}
		dartManager = agentdart.New(config)
		pullOptions = append(pullOptions, agentpull.WithDART(dartAddr))
		klog.InfoS("DART P2P gateway enabled", "addr", dartAddr,
			"discover", config.Discover, "cacheDir", config.CacheDir, "peerAdvertise", config.PeerAdvertise)
	}

	pull := &livePullClient{config: storeConfig, registry: registryProvider, options: pullOptions}
	if current, loadErr := storeConfig.Load(); loadErr == nil && current.Store == "" {
		klog.InfoS("artifact store not configured yet; pulls wait for the fast-sandbox-artifact-store ConfigMap",
			"mount", artifactstore.DefaultMountDir)
	}
	state, err := agentstate.New(stateRoot)
	if err != nil {
		return fmt.Errorf("open the lease state: %w", err)
	}
	defer func() { _ = state.Close() }()

	serviceOptions := []agentserver.ServiceOption{}
	if dartManager != nil {
		serviceOptions = append(serviceOptions, agentserver.WithDARTProbe(dartManager.Healthy))
	}
	service := agentserver.NewService(pull, state, stateRoot, serviceOptions...)
	server := agentserver.New(service, socketPath)
	klog.InfoS("firecracker-runtime-agent starting",
		"socket", socketPath, "artifactStoreMount", artifactstore.DefaultMountDir, "stateRoot", stateRoot,
		"registry", registryPath, "dart", dartAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if dartManager != nil {
		go func() {
			if err := dartManager.Run(ctx); err != nil {
				klog.ErrorS(err, "DART supervisor stopped with an error")
			}
		}()
	}
	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	klog.InfoS("firecracker-runtime-agent stopped")
	return nil
}

// livePullClient resolves the artifact store per call and rebuilds the pull
// client when the configuration changes: the store root and endpoint come
// from the mounted fast-sandbox-artifact-store ConfigMap, credentials from
// the mounted registry file. Both are kubelet-projected files that update in
// place, so an edit applies to the next pull without an agent restart.
type livePullClient struct {
	config   artifactstore.Loader
	registry registryconfig.Provider
	options  []agentpull.Option

	mu     sync.Mutex
	key    string
	client *agentpull.Client
}

// current returns the pull client for the current configuration, rebuilding
// it only when the resolved store, endpoint or credential changed.
func (l *livePullClient) current() (*agentpull.Client, error) {
	config, err := l.config.Load()
	if err != nil {
		return nil, err
	}
	if config.Store == "" {
		return nil, fmt.Errorf("artifact store is not configured (mount the fast-sandbox-artifact-store ConfigMap at %s)", artifactstore.DefaultMountDir)
	}
	credential, err := resolveCredential(l.registry, config.Store, config.Endpoint)
	if err != nil {
		return nil, err
	}
	key := strings.Join([]string{
		config.Store, config.Endpoint,
		credential.Username, credential.Password,
		credential.WriteUsername, credential.WritePassword, credential.Endpoint,
	}, "\x00")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil && l.key == key {
		return l.client, nil
	}
	client, err := agentpull.NewClient(config.Store, credential, l.options...)
	if err != nil {
		return nil, err
	}
	l.client, l.key = client, key
	klog.InfoS("artifact pull client configured", "store", config.Store, "endpoint", config.Endpoint)
	return client, nil
}

// PullImage pulls one image with a client built from the current config.
func (l *livePullClient) PullImage(ctx context.Context, stateRoot, image string) error {
	client, err := l.current()
	if err != nil {
		return err
	}
	return client.PullImage(ctx, stateRoot, image)
}

// PullCheckpoint materializes one instance-private checkpoint set addressed
// by manifest ref + digest with the current config.
func (l *livePullClient) PullCheckpoint(ctx context.Context, stateRoot, reference, manifestRef, artifactDigest string) error {
	client, err := l.current()
	if err != nil {
		return err
	}
	return client.PullCheckpoint(ctx, stateRoot, reference, manifestRef, artifactDigest)
}

// PublishImage publishes one snapshot artifact set with the current config;
// the service wires this path because the type implements artifactPublisher.
func (l *livePullClient) PublishImage(ctx context.Context, kind, key, dir string) (agentpull.PublishResult, error) {
	client, err := l.current()
	if err != nil {
		return agentpull.PublishResult{}, err
	}
	return client.PublishImage(ctx, kind, key, dir)
}

// dartListenAddress derives the DART client-plane listen address from the
// FAST_SANDBOX_DART_ADDR base (http://127.0.0.1:8145 -> 127.0.0.1:8145).
func dartListenAddress(dartAddr string) (string, error) {
	parsed, err := url.Parse(dartAddr)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid FAST_SANDBOX_DART_ADDR %q: expected http://host:port", dartAddr)
	}
	return parsed.Host, nil
}

// nodeHostID derives the stable node identity: the node hostname file
// (FAST_SANDBOX_HOSTNAME_FILE, mounted from the node by the DaemonSet),
// falling back to the process hostname.
func nodeHostID() string {
	path := getEnv("FAST_SANDBOX_HOSTNAME_FILE", "/etc/hostname")
	if payload, err := os.ReadFile(path); err == nil {
		if name := strings.TrimSpace(string(payload)); name != "" {
			return name
		}
	}
	return hostnameOrEmpty()
}

func hostnameOrEmpty() string {
	hostname, err := os.Hostname()
	if err != nil {
		return ""
	}
	return hostname
}

// resolveCredential matches the store endpoint host against the compiled
// registry configuration; the credential carries the read-only access key
// pair (Username/Password) and the store endpoint. When endpointEnv is set,
// its host is the matching key (an explicit artifact store endpoint such as
// a MinIO address); otherwise the store root host (the bucket) is matched.
// The match is a normalized host comparison (CredentialsForHost), not an
// image-reference match: a bare endpoint host like "127.0.0.1:9000" has no
// repository component for the reference-splitting rules to parse.
func resolveCredential(provider registryconfig.Provider, storeRoot, endpointEnv string) (registryconfig.Credential, error) {
	parsed, err := url.Parse(storeRoot)
	if err != nil {
		return registryconfig.Credential{}, fmt.Errorf("parse store root %q: %w", storeRoot, err)
	}
	matchHost := parsed.Host
	if endpointEnv != "" {
		parsedEndpoint, parseErr := url.Parse(endpointEnv)
		if parseErr != nil || parsedEndpoint.Host == "" {
			return registryconfig.Credential{}, fmt.Errorf("invalid artifact store endpoint %q", endpointEnv)
		}
		matchHost = parsedEndpoint.Host
	}
	if matchHost == "" {
		return registryconfig.Credential{}, fmt.Errorf("store root %q has no endpoint host", storeRoot)
	}
	var credential registryconfig.Credential
	var ok bool
	if hostMatcher, supports := provider.(interface {
		CredentialsForHost(string) (registryconfig.Credential, bool, error)
	}); supports {
		credential, ok, err = hostMatcher.CredentialsForHost(matchHost)
	} else {
		credential, ok, err = provider.Credentials(matchHost)
	}
	if err != nil {
		return registryconfig.Credential{}, err
	}
	if !ok {
		return registryconfig.Credential{}, fmt.Errorf("no read-only credential configured for store endpoint %q", matchHost)
	}
	return credential, nil
}

func getEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
