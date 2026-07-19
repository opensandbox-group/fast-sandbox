package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/fastlet/runtime"
	"fast-sandbox/internal/fastlet/server"
	"fast-sandbox/internal/runtimecatalog"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

func main() {
	flag.Parse()
	klog.Info("starting sandbox fastlet")

	podName := getEnv("POD_NAME", "")
	podUID := getEnv("POD_UID", "")
	podIP := getEnv("POD_IP", "")
	nodeName := getEnv("NODE_NAME", "")
	namespace := getEnv("NAMESPACE", "")
	fastletPort := getEnv("FASTLET_CONTROL_PORT", getEnv("AGENT_PORT", ":5758"))
	runtimeTypeStr := getEnv("FAST_SANDBOX_RUNTIME", "container")
	runtimeSocket := getEnv("RUNTIME_SOCKET", "")
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeName(runtimeTypeStr))
	if err != nil {
		klog.ErrorS(err, "Failed to resolve runtime profile")
		os.Exit(1)
	}
	injectedRuntimeHash := getEnv("FAST_SANDBOX_RUNTIME_PROFILE_HASH", runtimeProfile.ProfileHash)
	if injectedRuntimeHash != runtimeProfile.ProfileHash {
		klog.ErrorS(runtime.ErrSandboxProfileMismatch, "Injected runtime profile hash does not match built-in catalog", "injected", injectedRuntimeHash, "expected", runtimeProfile.ProfileHash)
		os.Exit(1)
	}
	resourceProfile, err := resourceProfileFromEnvironment()
	if err != nil {
		klog.ErrorS(err, "Failed to resolve Sandbox resource profile")
		os.Exit(1)
	}
	warmImages, err := warmImagesFromEnvironment()
	if err != nil {
		klog.ErrorS(err, "Failed to parse warmImages")
		os.Exit(1)
	}

	klog.InfoS("Fastlet Info", "PodName", podName, "PodIP", podIP, "NodeName", nodeName, "Namespace", namespace)
	klog.InfoS("Runtime", "Type", runtimeTypeStr, "Socket", runtimeSocket)

	ctx := context.Background()
	var rt runtime.RuntimeDriver

	rt, err = runtime.NewRuntime(ctx, runtime.RuntimeType(runtimeTypeStr), runtimeSocket)

	if err != nil {
		klog.ErrorS(err, "Failed to initialize runtime")
		os.Exit(1)
	}
	defer rt.Close()

	rt.SetNamespace(namespace)

	klog.InfoS("Runtime initialized successfully", "type", runtimeTypeStr)

	sandboxManager, err := runtime.NewSandboxManagerWithConfig(rt, runtime.SandboxManagerConfig{
		Capacity: capacityFromEnvironment(), RuntimeProfileHash: runtimeProfile.ProfileHash, ResourceProfile: &resourceProfile,
		FastletPodUID: podUID, RecoverOnStart: true,
		WarmImages: warmImages,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to initialize Sandbox manager")
		os.Exit(1)
	}
	defer sandboxManager.Close()
	go recoverUntilReady(ctx, sandboxManager)

	fastletServer := server.NewFastletServer(fastletPort, sandboxManager)
	klog.InfoS("Starting Fastlet HTTP Server", "port", fastletPort)

	if err := fastletServer.Start(); err != nil {
		klog.ErrorS(err, "Fastlet server failed")
		os.Exit(1)
	}
}

func recoverUntilReady(ctx context.Context, manager *runtime.SandboxManager) {
	for {
		if err := manager.Recover(ctx); err == nil {
			klog.Info("Fastlet runtime recovery completed")
			go func() {
				if err := manager.WarmCache(ctx); err != nil {
					klog.ErrorS(err, "Asynchronous warmImages preparation failed")
				}
			}()
			return
		} else {
			klog.ErrorS(err, "Fastlet runtime recovery failed; readiness remains false")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func warmImagesFromEnvironment() ([]string, error) {
	value := getEnv("FAST_SANDBOX_WARM_IMAGES", "[]")
	var images []string
	if err := json.Unmarshal([]byte(value), &images); err != nil {
		return nil, err
	}
	return images, nil
}

func resourceProfileFromEnvironment() (apiv1alpha1.SandboxResourceProfile, error) {
	cpu, err := resource.ParseQuantity(getEnv("FAST_SANDBOX_RESOURCE_CPU", "1"))
	if err != nil {
		return apiv1alpha1.SandboxResourceProfile{}, err
	}
	memory, err := resource.ParseQuantity(getEnv("FAST_SANDBOX_RESOURCE_MEMORY", "512Mi"))
	if err != nil {
		return apiv1alpha1.SandboxResourceProfile{}, err
	}
	pids, err := strconv.ParseInt(getEnv("FAST_SANDBOX_RESOURCE_PIDS", "256"), 10, 64)
	if err != nil {
		return apiv1alpha1.SandboxResourceProfile{}, err
	}
	return (apiv1alpha1.SandboxPoolSpec{SandboxResources: apiv1alpha1.SandboxResourceProfile{
		CPU: cpu, Memory: memory, PIDs: pids,
	}}).EffectiveSandboxResources()
}

func capacityFromEnvironment() int {
	value, err := strconv.Atoi(getEnv("FASTLET_CAPACITY", getEnv("AGENT_CAPACITY", "5")))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
