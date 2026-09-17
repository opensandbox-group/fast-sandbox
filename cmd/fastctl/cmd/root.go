package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"
	"fast-sandbox/internal/observability"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
)

const (
	fastPathEndpointEnv     = "FAST_SANDBOX_ENDPOINT"
	sandboxProxyEndpointEnv = "FAST_SANDBOX_PROXY_ENDPOINT"
)

var (
	cfgFile       string
	endpoint      string
	namespace     string
	proxyEndpoint string

	// traceShutdown terminates the OTLP exporter installed by Execute.
	// Command failure paths use it so spans are not lost on os.Exit.
	traceShutdown observability.Shutdown
)

// rootCmd represents the base command when called without any subcommands
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

	//  Flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./.fastctl/config.json)")
	rootCmd.PersistentFlags().StringVar(&endpoint, "endpoint", "localhost:9090", "Fast-Path gRPC endpoint (env: "+fastPathEndpointEnv+")")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "fast-sandbox", "Kubernetes resource namespace")
	rootCmd.PersistentFlags().StringVar(&proxyEndpoint, "proxy-endpoint", "", "Override the Sandbox Proxy authority (env: "+sandboxProxyEndpointEnv+")")

	mustBindConfigSources(viper.GetViper(), rootCmd.PersistentFlags())
}

// initConfig reads in config file and ENV variables if set.
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

	conn, err := grpc.Dial(ep,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor("fastctl")),
	)
	if err != nil {
		klog.ErrorS(err, "Failed to connect to gRPC endpoint", "endpoint", ep)
		return nil, nil, fmt.Errorf("failed to connect to %s: %v", ep, err)
	}
	klog.V(4).InfoS("successfully connected to gRPC endpoint", "endpoint", ep)
	return fastpathv2.NewFastPathServiceClient(conn), conn, nil
}

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
