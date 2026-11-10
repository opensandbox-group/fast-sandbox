package fastletproxy

import (
	"context"
	"errors"
	"net/http"

	dataplane "fast-sandbox/internal/dataplane/contract"
)

type RoutePublisher struct {
	client *ControlClient
}

func NewRoutePublisher(client *ControlClient) *RoutePublisher {
	return &RoutePublisher{client: client}
}

func (p *RoutePublisher) ApplyRoute(ctx context.Context, publication dataplane.RoutePublication) error {
	return p.client.Apply(ctx, routeFromPublication(publication))
}

func (p *RoutePublisher) RemoveRoute(ctx context.Context, publication dataplane.RoutePublication) error {
	err := p.client.MarkDraining(ctx, publication.SandboxUID, publication.RouteGeneration)
	var controlError *ControlError
	if err != nil && (!errors.As(err, &controlError) || controlError.StatusCode != http.StatusNotFound) {
		return err
	}
	err = p.client.Delete(ctx, publication.SandboxUID, publication.RouteGeneration)
	if errors.As(err, &controlError) && controlError.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (p *RoutePublisher) ReconcileRoutes(ctx context.Context, publications []dataplane.RoutePublication) error {
	snapshot, err := p.client.Snapshot(ctx)
	if err != nil {
		return err
	}
	desired := make(map[string]dataplane.RoutePublication, len(publications))
	for _, publication := range publications {
		desired[publication.SandboxUID] = publication
	}
	for _, route := range snapshot.Routes {
		if _, exists := desired[route.SandboxUID]; exists {
			continue
		}
		if err := p.client.Delete(ctx, route.SandboxUID, route.RouteGeneration); err != nil {
			return err
		}
	}
	for _, publication := range publications {
		if err := p.ResumeRoute(ctx, publication); err != nil {
			return err
		}
		if err := p.ApplyRoute(ctx, publication); err != nil {
			return err
		}
	}
	return nil
}

func routeFromPublication(publication dataplane.RoutePublication) Route {
	return Route{
		RouteKey: RouteKey{SandboxUID: publication.SandboxUID, RouteGeneration: publication.RouteGeneration},
		RouteSpec: RouteSpec{
			Namespace: publication.Namespace, FastletPodUID: publication.FastletPodUID,
			AssignmentAttempt: publication.AssignmentAttempt, Access: publication.Access,
			State: RouteReady, Components: publication.Components,
		},
	}
}

var _ dataplane.RoutePublisher = (*RoutePublisher)(nil)

func (p *RoutePublisher) SuspendRoute(ctx context.Context, publication dataplane.RoutePublication) error {
	return p.setSuspended(ctx, publication, true)
}
func (p *RoutePublisher) ResumeRoute(ctx context.Context, publication dataplane.RoutePublication) error {
	return p.setSuspended(ctx, publication, false)
}
func (p *RoutePublisher) setSuspended(ctx context.Context, publication dataplane.RoutePublication, suspended bool) error {
	snapshot, err := p.client.Snapshot(ctx)
	if err != nil {
		return err
	}
	return p.client.SetSuspended(ctx, publication.SandboxUID, publication.RouteGeneration, snapshot.Revision, suspended)
}

var _ dataplane.RecoveryRoutePublisher = (*RoutePublisher)(nil)
