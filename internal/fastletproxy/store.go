package fastletproxy

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
)

type RouteState string

const (
	RouteReady    RouteState = "Ready"
	RouteDraining RouteState = "Draining"
)

var (
	ErrRouteNotFound = errors.New("sandbox route not found")
	ErrRouteStale    = errors.New("stale sandbox route generation")
	ErrRouteConflict = errors.New("conflicting sandbox route")
	ErrRouteDraining = errors.New("sandbox route is draining")
)

// Route is the Fastlet-local, runtime-neutral route authority. Target ports
// are deliberately absent: one AccessDescriptor admits arbitrary validated
// ports and the signed credential narrows each external request.
type Route struct {
	Namespace         string                          `json:"namespace"`
	SandboxUID        string                          `json:"sandboxUid"`
	FastletPodUID     string                          `json:"fastletPodUid"`
	AssignmentAttempt int64                           `json:"assignmentAttempt"`
	RouteGeneration   int64                           `json:"routeGeneration"`
	Access            fastletnetwork.AccessDescriptor `json:"access"`
	State             RouteState                      `json:"state"`
	UpstreamHeaders   map[string]string               `json:"upstreamHeaders,omitempty"`
}

func (r Route) validate() error {
	if r.Namespace == "" || r.SandboxUID == "" || r.FastletPodUID == "" ||
		r.AssignmentAttempt <= 0 || r.RouteGeneration <= 0 || r.Access.Kind == "" {
		return fmt.Errorf("%w: incomplete route identity", ErrRouteConflict)
	}
	if r.State == "" {
		r.State = RouteReady
	}
	if r.State != RouteReady && r.State != RouteDraining {
		return fmt.Errorf("%w: invalid route state %q", ErrRouteConflict, r.State)
	}
	return nil
}

type EventType string

const (
	EventApplied  EventType = "Applied"
	EventDeleted  EventType = "Deleted"
	EventDraining EventType = "Draining"
)

type Event struct {
	Revision        uint64    `json:"revision"`
	Type            EventType `json:"type"`
	Route           *Route    `json:"route,omitempty"`
	SandboxUID      string    `json:"sandboxUid"`
	RouteGeneration int64     `json:"routeGeneration"`
}

type Snapshot struct {
	Revision uint64  `json:"revision"`
	Routes   []Route `json:"routes"`
}

// Store keeps generation tombstones after deletion. This prevents a delayed
// ApplyRoute from resurrecting an old runtime even though no active route is
// currently present.
type Store struct {
	mu          sync.RWMutex
	routes      map[string]Route
	highWater   map[string]int64
	revision    uint64
	nextWatcher uint64
	watchers    map[uint64]chan Event
}

func NewStore() *Store {
	return &Store{
		routes: make(map[string]Route), highWater: make(map[string]int64), watchers: make(map[uint64]chan Event),
	}
}

func (s *Store) Apply(route Route) (uint64, error) {
	if route.State == "" {
		route.State = RouteReady
	}
	if err := route.validate(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	high := s.highWater[route.SandboxUID]
	if route.RouteGeneration < high {
		return s.revision, ErrRouteStale
	}
	if existing, ok := s.routes[route.SandboxUID]; ok && route.RouteGeneration == existing.RouteGeneration {
		if reflect.DeepEqual(existing, route) {
			return s.revision, nil
		}
		return s.revision, ErrRouteConflict
	}
	if route.RouteGeneration == high && high != 0 {
		// Equal to a deletion tombstone, but no active route exists.
		return s.revision, ErrRouteStale
	}
	s.highWater[route.SandboxUID] = route.RouteGeneration
	s.routes[route.SandboxUID] = cloneRoute(route)
	s.revision++
	s.publishLocked(Event{Revision: s.revision, Type: EventApplied, Route: routePtr(route), SandboxUID: route.SandboxUID, RouteGeneration: route.RouteGeneration})
	return s.revision, nil
}

func (s *Store) MarkDraining(sandboxUID string, generation int64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	route, ok := s.routes[sandboxUID]
	if !ok {
		if generation < s.highWater[sandboxUID] {
			return s.revision, ErrRouteStale
		}
		return s.revision, ErrRouteNotFound
	}
	if generation < route.RouteGeneration {
		return s.revision, ErrRouteStale
	}
	if generation > route.RouteGeneration {
		return s.revision, ErrRouteConflict
	}
	if route.State == RouteDraining {
		return s.revision, nil
	}
	route.State = RouteDraining
	s.routes[sandboxUID] = route
	s.revision++
	s.publishLocked(Event{Revision: s.revision, Type: EventDraining, Route: routePtr(route), SandboxUID: sandboxUID, RouteGeneration: generation})
	return s.revision, nil
}

func (s *Store) Delete(sandboxUID string, generation int64) (uint64, error) {
	if sandboxUID == "" || generation <= 0 {
		return 0, ErrRouteConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	high := s.highWater[sandboxUID]
	if generation < high {
		return s.revision, ErrRouteStale
	}
	if generation == high {
		if _, exists := s.routes[sandboxUID]; !exists {
			return s.revision, nil
		}
	}
	s.highWater[sandboxUID] = generation
	delete(s.routes, sandboxUID)
	s.revision++
	s.publishLocked(Event{Revision: s.revision, Type: EventDeleted, SandboxUID: sandboxUID, RouteGeneration: generation})
	return s.revision, nil
}

func (s *Store) Lookup(sandboxUID string) (Route, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	route, ok := s.routes[sandboxUID]
	if !ok {
		return Route{}, ErrRouteNotFound
	}
	if route.State == RouteDraining {
		return Route{}, ErrRouteDraining
	}
	return cloneRoute(route), nil
}

func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	routes := make([]Route, 0, len(s.routes))
	for _, route := range s.routes {
		routes = append(routes, cloneRoute(route))
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].SandboxUID < routes[j].SandboxUID })
	return Snapshot{Revision: s.revision, Routes: routes}
}

func (s *Store) Subscribe() (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextWatcher++
	id := s.nextWatcher
	ch := make(chan Event, 64)
	s.watchers[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := s.watchers[id]; ok {
			delete(s.watchers, id)
			close(current)
		}
	}
}

func (s *Store) publishLocked(event Event) {
	for id, watcher := range s.watchers {
		select {
		case watcher <- event:
		default:
			delete(s.watchers, id)
			close(watcher)
		}
	}
}

func cloneRoute(route Route) Route {
	clone := route
	if route.UpstreamHeaders != nil {
		clone.UpstreamHeaders = make(map[string]string, len(route.UpstreamHeaders))
		for key, value := range route.UpstreamHeaders {
			clone.UpstreamHeaders[key] = value
		}
	}
	return clone
}

func routePtr(route Route) *Route {
	clone := cloneRoute(route)
	return &clone
}
