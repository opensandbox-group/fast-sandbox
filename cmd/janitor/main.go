package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/janitor"

	containerd "github.com/containerd/containerd/v2/client"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	var kubeconfig string
	var nodeName string
	var ctrdSocket string
	var orphanTimeout time.Duration
	var scanInterval time.Duration
	var networkStateRoot string
	var boxLiteStateRoot string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Name of the node this janitor is running on")
	flag.StringVar(&ctrdSocket, "containerd-socket", "/run/containerd/containerd.sock", "Path to containerd socket")
	flag.DurationVar(&orphanTimeout, "orphan-timeout", 30*time.Second, "Minimum age before an orphan resource can be cleaned")
	flag.DurationVar(&scanInterval, "scan-interval", 2*time.Minute, "Interval for full container scan")
	flag.StringVar(&networkStateRoot, "network-state-root", "/run/fast-sandbox/network", "Host-mounted Fastlet Linux network state root")
	flag.StringVar(&boxLiteStateRoot, "boxlite-state-root", "/var/lib/fast-sandbox/boxlite", "Host-mounted BoxLite state root")

	klog.InitFlags(nil)
	flag.Parse()

	if nodeName == "" {
		klog.ErrorS(nil, "node-name is required (or set NODE_NAME env)")
		os.Exit(1)
	}

	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		klog.ErrorS(err, "Failed to get kubeconfig")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.ErrorS(err, "Failed to create kubernetes clientset")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := apiv1alpha1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to register Sandbox API")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to register core API")
		os.Exit(1)
	}
	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		klog.ErrorS(err, "Failed to create generic k8s client")
		os.Exit(1)
	}

	ctrdClient, err := containerd.New(ctrdSocket)
	if err != nil {
		klog.ErrorS(err, "Failed to connect to containerd")
		os.Exit(1)
	}
	defer ctrdClient.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	j := janitor.NewJanitor(clientset, ctrdClient, nodeName)
	j.AddBackend(janitor.NewLinuxNetworkBackend(networkStateRoot, fastletnetwork.NewLinuxNetNSDriver(fastletnetwork.LinuxDriverConfig{})))
	j.AddBackend(janitor.NewBoxLiteBackend(boxLiteStateRoot))
	j.K8sClient = k8sClient
	j.OrphanTimeout = orphanTimeout
	j.ScanInterval = scanInterval
	klog.InfoS("Starting Janitor", "node", nodeName, "orphan-timeout", orphanTimeout, "scan-interval", scanInterval)
	if err := j.Run(ctx); err != nil {
		klog.ErrorS(err, "Janitor exited with error")
		os.Exit(1)
	}
}
