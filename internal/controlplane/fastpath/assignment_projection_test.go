package fastpath

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
)

// Project the initial assignment, then hold the previous projection across
// two reads after each CAS. This models a reconciler lagging consecutive
// proven-rejection candidate switches, without a timing-dependent goroutine.
type projectionLagClient struct {
	client.Client
	reads        map[int64]int
	stalled      bool
	change       bool
	getError     error
	cancel       context.CancelFunc
	beforeCreate func(client.ObjectKey)
}

func (c *projectionLagClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.beforeCreate != nil {
		c.beforeCreate(client.ObjectKeyFromObject(obj))
	}
	if err := c.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*apiv1alpha2.Sandbox); ok {
		_, err := assignment.ProjectAssignmentToStatus(ctx, c.Client, client.ObjectKeyFromObject(obj))
		return err
	}
	return nil
}
func (c *projectionLagClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	sandbox, ok := obj.(*apiv1alpha2.Sandbox)
	if !ok {
		return nil
	}
	envelope, err := assignment.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return err
	}
	if envelope == nil || sandbox.Status.Placement.Attempt == envelope.Attempt {
		return nil
	}
	c.reads[envelope.Attempt]++
	if c.reads[envelope.Attempt] >= 3 && c.cancel != nil {
		c.cancel()
		return ctx.Err()
	}
	if c.reads[envelope.Attempt] >= 3 && c.getError != nil {
		return c.getError
	}
	if c.reads[envelope.Attempt] < 3 {
		return nil
	}
	if c.change {
		changed := *envelope
		changed.Attempt += 10
		changed.RouteGeneration += 10
		changed.RuntimeInstanceID = "changed-concurrently"
		if err := assignment.SetAssignmentAnnotation(sandbox, changed); err != nil {
			return err
		}
		return c.Client.Update(ctx, sandbox)
	}
	if c.stalled {
		return nil
	}
	projected, err := assignment.ProjectAssignmentToStatus(ctx, c.Client, key)
	if err != nil {
		return err
	}
	*sandbox = *projected
	return nil
}
func projectionLagServer(t *testing.T) (*Server, *projectionLagClient, *fastpathFastlet) {
	t.Helper()
	server, k8s, registry, fastlet := newV2Server(t)
	registry.candidates = nil
	fastlet.createFailures = map[string]error{}
	for i := 0; i < 4; i++ {
		candidate := testCandidate(string(rune('a'+i)), string(rune('a'+i)), "10.0.0."+string(rune('1'+i)))
		registry.candidates = append(registry.candidates, candidate)
		registry.fastlets[placement.FastletID(candidate.ID)] = candidate
		if i < 3 {
			fastlet.createFailures[candidate.PodIP] = capacityRejection()
		}
	}
	lag := &projectionLagClient{Client: k8s, reads: map[int64]int{}}
	server.K8sClient = lag
	return server, lag, fastlet
}
func TestCreateWaitsForLaggingProjectionAcrossRepeatedCandidateSwitches(t *testing.T) {
	server, lag, fastlet := projectionLagServer(t)
	_, err := server.CreateSandbox(context.Background(), createRequest("projection-lag"))
	require.NoError(t, err)
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}, fastlet.createIPs)
	require.GreaterOrEqual(t, lag.reads[2], 3)
	require.GreaterOrEqual(t, lag.reads[3], 3)
	for i, req := range fastlet.createRequests {
		require.Equal(t, int64(i+1), req.Identity.AssignmentAttempt)
		require.Equal(t, int64(1), req.Identity.InstanceGeneration)
	}
}
func TestProjectionWaitStopsOnChangedAssignment(t *testing.T) {
	server, lag, fastlet := projectionLagServer(t)
	lag.change = true
	_, err := server.CreateSandbox(context.Background(), createRequest("projection-changed"))
	require.Equal(t, codes.Aborted, status.Code(err))
	require.Contains(t, err.Error(), assignment.ErrAssignmentAnnotationChanged.Error())
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, fastlet.createIPs)
	require.Equal(t, 3, lag.reads[2], "a changed annotation must not be polled as a lagging projection")
}
func TestProjectionWaitHonorsCallerDeadline(t *testing.T) {
	server, lag, fastlet := projectionLagServer(t)
	lag.stalled = true
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := server.CreateSandbox(ctx, createRequest("projection-deadline"))
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, fastlet.createIPs)
}
func TestProjectionWaitIsBoundedAndRetainsIntent(t *testing.T) {
	server, lag, fastlet := projectionLagServer(t)
	lag.stalled = true
	start := time.Now()
	_, err := server.CreateSandbox(context.Background(), createRequest("projection-stalled"))
	require.Equal(t, codes.Aborted, status.Code(err))
	require.Contains(t, err.Error(), assignment.ErrAssignmentProjectionConflict.Error())
	require.Less(t, time.Since(start), 3*time.Second)
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, fastlet.createIPs)
	var persisted apiv1alpha2.Sandbox
	require.NoError(t, lag.Client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "projection-stalled"}, &persisted))
	envelope, err := assignment.AssignmentFromAnnotation(&persisted)
	require.NoError(t, err)
	require.Equal(t, int64(2), envelope.Attempt)
	require.Equal(t, int64(1), persisted.Status.Placement.Attempt)
}

func TestProjectionSwitchHonorsCancellation(t *testing.T) {
	server, lag, fastlet := projectionLagServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lag.cancel = cancel
	_, err := server.CreateSandbox(ctx, createRequest("projection-canceled"))
	require.Equal(t, codes.Canceled, status.Code(err))
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, fastlet.createIPs)
}
func TestProjectionSwitchDoesNotRetryUnrelatedReadError(t *testing.T) {
	server, lag, fastlet := projectionLagServer(t)
	lag.getError = errors.New("unrelated API read error")
	_, err := server.CreateSandbox(context.Background(), createRequest("projection-read-error"))
	require.Equal(t, codes.Aborted, status.Code(err))
	require.Contains(t, err.Error(), "unrelated API read error")
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, fastlet.createIPs)
	require.Equal(t, 3, lag.reads[2])
}

func TestCreateOwnershipExistsBeforePersistenceAndReleasesOnExit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			server, lag, _ := projectionLagServer(t)
			lag.stalled = fail
			called := false
			lag.beforeCreate = func(key client.ObjectKey) {
				called = true
				require.True(t, server.Orchestrator.FastPathCreateActive(key))
			}
			_, err := server.CreateSandbox(context.Background(), createRequest("create-ownership"))
			require.Equal(t, fail, err != nil)
			require.True(t, called)
			require.False(t, server.Orchestrator.FastPathCreateActive(client.ObjectKey{Namespace: "default", Name: "create-ownership"}))
		})
	}
}
