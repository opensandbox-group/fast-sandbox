package infra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

func TestStorePathsUsesConfiguredKubeletRoot(t *testing.T) {
	podRoot, hostRoot, err := StorePaths("pod-uid", "/srv/kubelet")
	require.NoError(t, err)
	require.Equal(t, "/opt/fast-sandbox/infra", podRoot)
	require.Equal(t,
		"/srv/kubelet/pods/pod-uid/volumes/kubernetes.io~empty-dir/infra-tools",
		hostRoot,
	)

	_, _, err = StorePaths("pod-uid", "relative/kubelet")
	require.EqualError(t, err, "kubelet root must be an absolute path")
}

func TestManagerPreparesInlinePlanAndSupervisorOnce(t *testing.T) {
	manager, resolver := testManager(t, apiv1alpha2.RuntimeContainer)

	require.NoError(t, manager.Prepare(context.Background()))
	plan, err := manager.Plan()
	require.NoError(t, err)
	require.NotNil(t, plan.Supervisor)
	require.Len(t, plan.Components, 1)
	require.Len(t, plan.Components[0].Mappings, 2)
	require.NoError(t, manager.Prepare(context.Background()))
	require.Equal(t, 1, resolver.calls)
	require.Len(t, manager.ArtifactReferences(), 2)
}

func TestManagerRetriesTransientArtifactPreparationFailure(t *testing.T) {
	manager, resolver := testManager(t, apiv1alpha2.RuntimeContainer)
	resolver.failFirst = true

	require.Error(t, manager.Prepare(context.Background()))
	require.NoError(t, manager.Prepare(context.Background()))
	_, err := manager.Plan()
	require.NoError(t, err)
	require.Equal(t, 2, resolver.calls)
}

func TestManagerPreparesHostProcessComponentWithoutArtifactOrSupervisor(t *testing.T) {
	manager, resolver := testHostProcessManager(t)
	require.NoError(t, manager.Prepare(context.Background()))
	plan, err := manager.Plan()
	require.NoError(t, err)
	require.Nil(t, plan.Supervisor)
	require.Len(t, plan.Components, 1)
	require.Equal(t, runtimecatalog.InfraDeliveryHostProcess, plan.Components[0].Plan.Delivery)
	require.Empty(t, plan.Components[0].Mappings)
	require.Zero(t, resolver.calls, "host-process components must not resolve artifacts")
	require.Empty(t, manager.ArtifactReferences())
}

func testHostProcessManager(t *testing.T) (*Manager, *testResolver) {
	t.Helper()
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	component := apiv1alpha2.InfraComponent{
		Name:     "egress",
		Delivery: apiv1alpha2.InfraDeliveryHostProcess,
		Process: apiv1alpha2.InfraProcess{
			Command:     []string{"/bin/egress"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{TCPConnect: &apiv1alpha2.InfraTCPConnect{}, TimeoutSeconds: 5},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 18080},
	}
	plan, err := infracatalog.Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
	require.NoError(t, err)
	resolver := &testResolver{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		Plan: plan, RuntimeProfile: runtimeProfile, Store: store, Resolver: resolver,
		SandboxInitPath: filepath.Join(root, "sandbox-init"),
	})
	require.NoError(t, err)
	return manager, resolver
}

type testResolver struct {
	source    PreparedSource
	calls     int
	failFirst bool
}

func (r *testResolver) Prepare(context.Context, infracatalog.ArtifactSource, *ArtifactStore) (PreparedSource, error) {
	r.calls++
	if r.failFirst && r.calls == 1 {
		return PreparedSource{}, errors.New("temporary registry failure")
	}
	return r.source, nil
}

func testManager(t *testing.T, runtimeName apiv1alpha2.RuntimeName) (*Manager, *testResolver) {
	t.Helper()
	root := t.TempDir()
	podRoot := filepath.Join(root, "pod")
	hostRoot := filepath.Join(root, "host")
	sourcePodRoot := filepath.Join(podRoot, "source")
	sourceHostRoot := filepath.Join(hostRoot, "source")
	require.NoError(t, os.MkdirAll(filepath.Join(sourcePodRoot, "config"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sourcePodRoot, "execd"), []byte("execd"), 0555))
	require.NoError(t, os.WriteFile(filepath.Join(sourcePodRoot, "config", "default.yaml"), []byte("enabled: true"), 0444))
	store, err := NewArtifactStore(podRoot, hostRoot)
	require.NoError(t, err)
	sandboxInit := filepath.Join(root, "sandbox-init")
	require.NoError(t, os.WriteFile(sandboxInit, []byte("sandbox-init"), 0555))
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(runtimeName)
	require.NoError(t, err)
	plan := testInfraPlan(t, runtimeProfile)
	resolver := &testResolver{source: PreparedSource{
		Digest: plan.Components[0].Artifact.Source.Digest, PodRoot: sourcePodRoot, HostRoot: sourceHostRoot,
	}}
	manager, err := NewManagerWithConfig(ManagerConfig{
		Plan: plan, RuntimeProfile: runtimeProfile, Store: store, Resolver: resolver,
		SandboxInitPath: sandboxInit,
	})
	require.NoError(t, err)
	return manager, resolver
}

func testInfraPlan(t *testing.T, runtimeProfile runtimecatalog.RuntimeProfile) infracatalog.Plan {
	t.Helper()
	component := apiv1alpha2.InfraComponent{
		Name: "execd",
		Artifact: &apiv1alpha2.InfraArtifact{
			Source: apiv1alpha2.InfraArtifactSource{Image: &apiv1alpha2.InfraArtifactImage{
				Reference: "registry.example/execd@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
			Mappings: []apiv1alpha2.InfraArtifactMapping{
				{SourcePath: "/execd", TargetPath: "/.fast/components/execd/execd"},
				{SourcePath: "/config", TargetPath: "/.fast/components/execd/config"},
			},
		},
		Process: apiv1alpha2.InfraProcess{
			Command: []string{"/.fast/components/execd/execd", "--port", "44772"},
			Env:     map[string]string{"MODE": "production"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{
				HTTPGet: &apiv1alpha2.InfraHTTPGet{Path: "/ping"},
			},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}
	plan, err := infracatalog.Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
	require.NoError(t, err)
	return plan
}
