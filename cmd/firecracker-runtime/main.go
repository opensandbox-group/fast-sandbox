package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifactstore"
	"fast-sandbox/internal/observability"
	"fast-sandbox/internal/registryconfig"
	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
	agentdart "fast-sandbox/internal/runtime/firecracker/agent/dart"
	agenthostready "fast-sandbox/internal/runtime/firecracker/agent/hostready"
	agentserver "fast-sandbox/internal/runtime/firecracker/agent/server"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"
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
		klog.ErrorS(err, "firecracker-runtime failed")
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

// run assembles the agent. It returns an error when the agent cannot serve.
// Deferred cleanup (server stop, lease state close) runs only on a normal
// return — the error path exits via os.Exit(1) in main after a klog.Flush.
// The dart child (dart mode) stops when its Run goroutine observes the
// signal context canceled (SIGTERM/SIGINT), not via defer.
func run() error {
	klog.InitFlags(nil)
	flag.Parse()
	shutdownTracing, err := observability.Configure(context.Background(), "firecracker-runtime")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()
	// All tunables live in the mounted agent.yaml (the
	// fast-sandbox-firecracker-runtime-config ConfigMap); the environment
	// only carries the Downward API identities (POD_UID, NODE_NAME,
	// NODE_IP) and the config path.
	configPath := getEnv("FAST_SANDBOX_AGENT_CONFIG", defaultConfigPath)
	config, err := loadAgentConfig(configPath)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(configPath); statErr != nil {
		klog.InfoS("agent config file not found; running on built-in defaults",
			"path", configPath)
	}
	klog.InfoS("agent config loaded", "path", configPath,
		"p2p", config.p2pMode(), "nodeReadiness", config.NodeReadiness.Enabled)
	socketPath := config.Socket
	stateRoot := config.StateRoot
	// The artifact store is read from the mounted fast-sandbox-artifact-store
	// ConfigMap on every pull, so a ConfigMap edit applies to the next pull
	// without an agent restart.
	storeConfig := artifactstore.Loader{}

	registryProvider := registryconfig.NewFileProvider(config.RegistryConfig)
	serviceOptions := []agentserver.ServiceOption{}

	// P2P distribution: exactly one of direct S3 (default), the node-local
	// dart child (p2p.dart.addr) or an external gateway (p2p.gateway.addr)
	// is active. Artifact bytes always fall back to direct S3 when the
	// gateway is unreachable.
	var pullOptions []agentpull.Option
	var dartManager *agentdart.Manager
	var p2pProbe func() bool
	var gatewayProbers []*gatewayProber
	if gateway := config.P2P.Gateway; gateway.Addr != "" {
		pullOptions = append(pullOptions, agentpull.WithPeerGateway(gateway.Addr, gateway.RoutePrefix))
		prober := newGatewayProber(gateway.Addr)
		gatewayProbers = append(gatewayProbers, prober)
		p2pProbe = prober.Healthy
		klog.InfoS("P2P gateway enabled (external provider)", "addr", gateway.Addr,
			"routePrefix", gateway.RoutePrefix)
	} else if config.P2P.Dart.Addr != "" {
		listen, err := dartListenAddress(config.P2P.Dart.Addr)
		if err != nil {
			return err
		}
		// A stable identity anchors the HRW keyspace AND names the per-node
		// block cache: the StateRoot can be shared (multi-node kind mounts
		// one host filesystem into every node container), but two dart
		// arenas must never point at the same directory. The node's
		// hostname is read from the mounted node hostname file because a
		// regular pod's own hostname is its pod name, which changes on
		// restart.
		nodeID := config.P2P.Dart.SelfID
		if nodeID == "" {
			nodeID = nodeHostID(config.HostnameFile)
		}
		peerPort := "9000"
		dartConfig := agentdart.Config{
			Binary:    config.P2P.Dart.Bin,
			Listen:    listen,
			Admin:     config.P2P.Dart.Admin,
			CacheDir:  filepath.Join(stateRoot, "cache", "p2p-"+strings.ReplaceAll(nodeID, "/", "-")),
			CacheSize: config.P2P.Dart.CacheSize,
			Discover:  config.P2P.Dart.Discover,
			SelfID:    nodeID,
			Log:       os.Stderr,
		}
		if nodeIP := getEnv("FAST_SANDBOX_NODE_IP", ""); nodeIP != "" {
			dartConfig.PeerAdvertise = net.JoinHostPort(nodeIP, peerPort)
		}
		dartManager = agentdart.New(dartConfig)
		pullOptions = append(pullOptions, agentpull.WithPeerGateway(config.P2P.Dart.Addr, ""))
		p2pProbe = dartManager.Healthy
		klog.InfoS("P2P gateway enabled (node-local dart)", "addr", config.P2P.Dart.Addr,
			"discover", dartConfig.Discover, "cacheDir", dartConfig.CacheDir, "peerAdvertise", dartConfig.PeerAdvertise)
	}

	// Node readiness (host checks + Firecracker asset install + scheduling
	// labels + FirecrackerReady condition). Enabled by config
	// (nodeReadiness.enabled) plus FAST_SANDBOX_NODE_NAME: the DaemonSet
	// injects spec.nodeName, so in-cluster deployments run the readiness
	// loop while bare-process runs (chain E2E) stay check-free. The
	// nodeReadiness section is re-read before every pass (hot reload of
	// thresholds/interval/asset source through the mounted ConfigMap), so
	// the static config below only seeds the manager until the first
	// reload.
	var hostReadyManager *agenthostready.Manager
	if nodeName := getEnv("FAST_SANDBOX_NODE_NAME", ""); nodeName != "" && config.NodeReadiness.Enabled {
		hostReadyManager = agenthostready.NewManager(agenthostready.ManagerConfig{
			NodeName: nodeName,
			Client:   newNodeClient(),
			Settings: readinessSettings(configPath, stateRoot),
		})
		serviceOptions = append(serviceOptions, agentserver.WithHostReadyProbe(hostReadyManager.Health))
		klog.InfoS("node readiness manager enabled", "node", nodeName,
			"assetsDir", config.NodeReadiness.AssetsDir, "config", configPath)
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

	if p2pProbe != nil {
		serviceOptions = append(serviceOptions, agentserver.WithP2PProbe(p2pProbe))
	}
	service := agentserver.NewService(pull, state, stateRoot, serviceOptions...)
	server := agentserver.New(service, socketPath)
	klog.InfoS("firecracker-runtime starting",
		"socket", socketPath, "artifactStoreMount", artifactstore.DefaultMountDir, "stateRoot", stateRoot,
		"registry", config.RegistryConfig, "p2p", config.p2pMode())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if dartManager != nil {
		go func() {
			if err := dartManager.Run(ctx); err != nil {
				klog.ErrorS(err, "P2P supervisor stopped with an error")
			}
		}()
	}
	for _, prober := range gatewayProbers {
		go prober.Run(ctx)
	}
	if hostReadyManager != nil {
		go hostReadyManager.Run(ctx)
	}
	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	klog.InfoS("firecracker-runtime stopped")
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

// dartListenAddress derives the dart client-plane listen address from the
// configured p2p.dart.addr base (http://127.0.0.1:8145 -> 127.0.0.1:8145).
func dartListenAddress(dartAddr string) (string, error) {
	parsed, err := url.Parse(dartAddr)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid p2p.dart.addr %q: expected http://host:port", dartAddr)
	}
	return parsed.Host, nil
}

// gatewayProbeInterval is the external-gateway probe cadence (mirrors the
// dart manager's admin-plane probe loop).
const gatewayProbeInterval = 2 * time.Second

// healthProbeTimeout bounds one gateway probe request.
const healthProbeTimeout = time.Second

// gatewayProber periodically probes an external P2P gateway and caches the
// verdict, so Health never blocks on the gateway (the dart manager follows
// the same background-probe pattern). Any non-5xx answer counts as up: 4xx
// on the bare base URL still proves the gateway serves; 5xx means it is
// failing requests and pulls fall back to direct S3.
type gatewayProber struct {
	base string
	http *http.Client
	up   atomic.Bool
}

func newGatewayProber(base string) *gatewayProber {
	return &gatewayProber{base: base, http: &http.Client{Timeout: healthProbeTimeout}}
}

// Healthy reports the last probe verdict.
func (p *gatewayProber) Healthy() bool { return p.up.Load() }

// Run probes until ctx is done.
func (p *gatewayProber) Run(ctx context.Context) {
	ticker := time.NewTicker(gatewayProbeInterval)
	defer ticker.Stop()
	for {
		p.probe()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *gatewayProber) probe() {
	response, err := p.http.Get(p.base)
	if err != nil {
		p.up.Store(false)
		return
	}
	_ = response.Body.Close()
	p.up.Store(response.StatusCode < http.StatusInternalServerError)
}

// nodeHostID derives the stable node identity from the configured node
// hostname file (mounted from the node by the DaemonSet), falling back to
// the process hostname.
func nodeHostID(hostnameFile string) string {
	if payload, err := os.ReadFile(hostnameFile); err == nil {
		if name := strings.TrimSpace(string(payload)); name != "" {
			return name
		}
	}
	return hostnameOrEmpty()
}

// newNodeClient builds the in-cluster node client (the DaemonSet
// ServiceAccount grants node get/patch).
func newNodeClient() agenthostready.NodeClient {
	inCluster, err := rest.InClusterConfig()
	if err != nil {
		klog.ErrorS(err, "no in-cluster identity; node readiness runs checks-only (no labels/condition)")
		return nil
	}
	clientset, err := kubernetes.NewForConfig(inCluster)
	if err != nil {
		klog.ErrorS(err, "build the node client failed; node readiness runs checks-only (no labels/condition)")
		return nil
	}
	return agenthostready.ClientsetNodeClient{Clientset: clientset}
}

// readinessSettings builds the hot-reload callback: the mounted config
// file is re-read before every readiness pass, so nodeReadiness edits
// (thresholds, interval, asset source) land without a restart. It seeds
// the whole manager config (the manager treats the Settings func as the
// source of truth), with two pins:
//   - the check StateRoot is forced to the startup value: the pull layer
//     serves the startup StateRoot and the readiness check must judge the
//     same tree;
//   - an invalid nodeReadiness value surfaces as a reload error (the
//     manager keeps the previous settings and logs), never a crash.
func readinessSettings(configPath, stateRoot string) func() (agenthostready.Settings, error) {
	return func() (agenthostready.Settings, error) {
		config, err := loadAgentConfig(configPath)
		if err != nil {
			return agenthostready.Settings{}, err
		}
		readiness := config.NodeReadiness
		minFree := agenthostready.DefaultMinFreeBytes
		if readiness.MinFree != "" {
			if minFree, err = agenthostready.ParseBytes(readiness.MinFree); err != nil {
				return agenthostready.Settings{}, fmt.Errorf("nodeReadiness.minFree: %w", err)
			}
		}
		minMemory := agenthostready.DefaultMinMemBytes
		if readiness.MinMemory != "" {
			if minMemory, err = agenthostready.ParseBytes(readiness.MinMemory); err != nil {
				return agenthostready.Settings{}, fmt.Errorf("nodeReadiness.minMemory: %w", err)
			}
		}
		var interval time.Duration
		if readiness.Interval != "" {
			if interval, err = time.ParseDuration(readiness.Interval); err != nil || interval <= 0 {
				return agenthostready.Settings{}, fmt.Errorf("nodeReadiness.interval %q: expected a positive duration like 5m", readiness.Interval)
			}
		}
		return agenthostready.Settings{
			Check: agenthostready.CheckConfig{
				StateRoot:    stateRoot,
				AssetsDir:    readiness.AssetsDir,
				MinFreeBytes: minFree,
				MinMemBytes:  minMemory,
			},
			Assets: &agenthostready.AssetConfig{
				Dir:       readiness.AssetsDir,
				FCVersion: readiness.FCVersion,
				KernelURL: readiness.KernelURL,
			},
			Interval: interval,
		}, nil
	}
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
