package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	"fast-sandbox/internal/observability"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
)

var (
	cfgFile       string
	endpoint      string
	namespace     string
	proxyEndpoint string
	infraAdapter  string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "fastctl",
	Short: "Fast Sandbox Control - High performance container management",
	Long: `fastctl is the official CLI for Fast Sandbox.
It provides a developer-friendly interface to manage sandboxes with millisecond latency.`,
}

func Execute() {
	traceShutdown, traceErr := observability.Configure(context.Background(), "fastctl")
	if traceErr != nil {
		fmt.Fprintln(os.Stderr, "configure OpenTelemetry:", traceErr)
		os.Exit(1)
	}
	if err := rootCmd.Execute(); err != nil {
		shutdownTracing(traceShutdown)
		fmt.Println(err)
		os.Exit(1)
	}
	shutdownTracing(traceShutdown)
}

func init() {
	cobra.OnInitialize(initConfig)

	//  Flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./.fastctl/config.json)")
	rootCmd.PersistentFlags().StringVar(&endpoint, "endpoint", "localhost:9090", "Controller gRPC endpoint")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	rootCmd.PersistentFlags().StringVar(&proxyEndpoint, "proxy-endpoint", "", "Override the Sandbox Proxy authority (for example, a local port-forward)")
	rootCmd.PersistentFlags().StringVar(&infraAdapter, "adapter", "execd", "Injected Infra Component protocol adapter")

	viper.BindPFlag("endpoint", rootCmd.PersistentFlags().Lookup("endpoint"))
	viper.BindPFlag("namespace", rootCmd.PersistentFlags().Lookup("namespace"))
	viper.BindPFlag("proxy-endpoint", rootCmd.PersistentFlags().Lookup("proxy-endpoint"))
	viper.BindPFlag("adapter", rootCmd.PersistentFlags().Lookup("adapter"))
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
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

	viper.AutomaticEnv() // read in environment variables that match

	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

var clientFactory = defaultClientFactory

func defaultClientFactory() (fastpathv1.FastPathServiceClient, *grpc.ClientConn, error) {
	ep := viper.GetString("endpoint")
	klog.V(4).InfoS("Creating gRPC client connection", "endpoint", ep)

	conn, err := grpc.Dial(ep,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor("fastctl")),
	)
	if err != nil {
		klog.ErrorS(err, "Failed to connect to gRPC endpoint", "endpoint", ep)
		return nil, nil, fmt.Errorf("failed to connect to %s: %v", ep, err)
	}
	klog.V(4).InfoS("Successfully connected to gRPC endpoint", "endpoint", ep)
	return fastpathv1.NewFastPathServiceClient(conn), conn, nil
}

func getClient() (fastpathv1.FastPathServiceClient, *grpc.ClientConn) {
	client, conn, err := clientFactory()
	if err != nil {
		log.Fatalf("Error: %v", err)
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
