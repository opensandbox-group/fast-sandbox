package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	fastpathv2 "fast-sandbox/api/proto/v2"
	"fast-sandbox/internal/fastletauth"
	"fast-sandbox/internal/observability"
)

const (
	fastPathEndpointEnv     = "FAST_SANDBOX_ENDPOINT"
	sandboxProxyEndpointEnv = "FAST_SANDBOX_PROXY_ENDPOINT"
)

var (
	cfgFile                              string
	endpoint                             string
	namespace                            string
	proxyEndpoint                        string
	authEnabled                          bool
	kubeconfig, tlsCAFile, tlsServerName string

	// traceShutdown terminates the OTLP exporter installed by Execute.
	// Command failure paths use it so spans are not lost on os.Exit.
	traceShutdown observability.Shutdown
)

var rootCmd = &cobra.Command{
	Use:   "fastctl",
	Short: "Fast Sandbox Control - High performance container management",
	Long: `fastctl is the official CLI for Fast Sandbox.
It provides a developer-friendly interface to manage sandboxes with millisecond latency.`,
}

func Execute() {
	shutdown, traceErr := observability.Configure(context.Background(), "fastctl")
	if traceErr != nil {
		fmt.Fprintln(os.Stderr, "configure OpenTelemetry:", traceErr)
		os.Exit(1)
	}
	traceShutdown = shutdown
	if err := rootCmd.Execute(); err != nil {
		exitWithError(err)
	}
	shutdownTracing(shutdown)
}

// exitWithError reports a command failure and terminates with status 1.
// os.Exit skips deferred cleanups, so the OTLP exporter and klog buffers
// are flushed here first; the message goes to stderr exactly like the
// error path of Execute.
func exitWithError(err error) {
	if traceShutdown != nil {
		shutdownTracing(traceShutdown)
	}
	klog.Flush()
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

// exitWithErrorf is exitWithError for usage and input-validation failures
// that carry no underlying error value.
func exitWithErrorf(format string, args ...any) {
	if traceShutdown != nil {
		shutdownTracing(traceShutdown)
	}
	klog.Flush()
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./.fastctl/config.json)")
	rootCmd.PersistentFlags().StringVar(&endpoint, "endpoint", "localhost:9090", "Fast-Path gRPC endpoint (env: "+fastPathEndpointEnv+")")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "fast-sandbox", "Kubernetes resource namespace")
	rootCmd.PersistentFlags().StringVar(&proxyEndpoint, "proxy-endpoint", "", "Override the Sandbox Proxy authority (env: "+sandboxProxyEndpointEnv+")")

	rootCmd.PersistentFlags().BoolVar(&authEnabled, "auth-enabled", false, "Enable FastPath caller authentication and TLS (default false; set true for authenticated TLS endpoints)")
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Kubernetes bootstrap kubeconfig (default loading rules when empty)")
	rootCmd.PersistentFlags().StringVar(&tlsCAFile, "tls-ca-file", "", "FastPath serving CA PEM bundle (empty uses system roots)")
	rootCmd.PersistentFlags().StringVar(&tlsServerName, "tls-server-name", "", "Expected serving certificate DNS name (empty uses endpoint hostname)")
	mustBindConfigSources(viper.GetViper(), rootCmd.PersistentFlags())
}

func initConfig() {
	// Tests and embedders may reset Viper after package initialization.
	// Rebinding here also makes the precedence explicit at command execution:
	// changed flags, endpoint environment variables, config file, flag defaults.
	mustBindConfigSources(viper.GetViper(), rootCmd.PersistentFlags())

	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.AddConfigPath("./.fastctl")
		home, err := os.UserHomeDir()
		if err == nil {
			viper.AddConfigPath(home + "/.fastctl")
		}
		viper.SetConfigName("config")
		viper.SetConfigType("json")
	}

	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

func bindConfigSources(config *viper.Viper, flags *pflag.FlagSet) error {
	for _, binding := range []struct {
		key  string
		flag string
	}{
		{key: "endpoint", flag: "endpoint"},
		{key: "namespace", flag: "namespace"},
		{key: "proxy-endpoint", flag: "proxy-endpoint"},
	} {
		if err := config.BindPFlag(binding.key, flags.Lookup(binding.flag)); err != nil {
			return fmt.Errorf("bind --%s: %w", binding.flag, err)
		}
	}
	if err := config.BindEnv("endpoint", fastPathEndpointEnv); err != nil {
		return fmt.Errorf("bind %s: %w", fastPathEndpointEnv, err)
	}
	if err := config.BindEnv("proxy-endpoint", sandboxProxyEndpointEnv); err != nil {
		return fmt.Errorf("bind %s: %w", sandboxProxyEndpointEnv, err)
	}
	return nil
}

func mustBindConfigSources(config *viper.Viper, flags *pflag.FlagSet) {
	if err := bindConfigSources(config, flags); err != nil {
		panic(err)
	}
}

var clientFactory = defaultClientFactory

func defaultClientFactory() (fastpathv2.FastPathServiceClient, *grpc.ClientConn, error) {
	ep := viper.GetString("endpoint")
	klog.V(4).InfoS("creating gRPC client connection", "endpoint", ep)

	if !authEnabled {
		conn, err := grpc.NewClient(ep,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor("fastctl")),
		)
		if err != nil {
			klog.ErrorS(err, "Failed to connect to gRPC endpoint", "endpoint", ep)
			return nil, nil, fmt.Errorf("failed to connect to %s: %w", ep, err)
		}
		return fastpathv2.NewFastPathServiceClient(conn), conn, nil
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load Kubernetes bootstrap credentials: %w", err)
	}
	if config.Insecure || !strings.HasPrefix(config.Host, "https://") {
		return nil, nil, fmt.Errorf("FastPath TokenRequest requires verified Kubernetes API TLS")
	}
	api, err := corev1.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}
	callerNamespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	if callerNamespace == "" {
		callerNamespace = "fast-sandbox-system"
	}
	source := fastletauth.NewTokenRequestSource(func(ctx context.Context, ns, name string, request *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		return api.ServiceAccounts(ns).CreateToken(ctx, name, request, metav1.CreateOptions{})
	}, fastletauth.PodServiceAccountIdentity{Namespace: callerNamespace, ServiceAccount: "opensandbox-server"}, "fast-sandbox-fastpath")
	var roots *x509.CertPool
	if tlsCAFile != "" {
		pem, err := os.ReadFile(tlsCAFile)
		if err != nil {
			return nil, nil, err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("FastPath CA bundle contains no certificates")
		}
	}
	conn, err := grpc.NewClient(ep,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: tlsServerName})),
		grpc.WithPerRPCCredentials(callerCredentials{source}),
		grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor("fastctl")),
	)
	if err != nil {
		klog.ErrorS(err, "Failed to connect to gRPC endpoint", "endpoint", ep)
		return nil, nil, fmt.Errorf("failed to connect to %s: %w", ep, err)
	}
	klog.V(4).InfoS("successfully connected to gRPC endpoint", "endpoint", ep)
	return fastpathv2.NewFastPathServiceClient(conn), conn, nil
}

type callerCredentials struct {
	source *fastletauth.TokenRequestSource
}

func (c callerCredentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token, err := c.source.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("FastPath caller TokenRequest failed; check bootstrap credentials and caller RBAC")
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (callerCredentials) RequireTransportSecurity() bool { return true }

func getClient() (fastpathv2.FastPathServiceClient, *grpc.ClientConn) {
	client, conn, err := clientFactory()
	if err != nil {
		exitWithError(err)
	}
	return client, conn
}

func shutdownTracing(shutdown observability.Shutdown) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "flush OpenTelemetry traces:", err)
	}
}
