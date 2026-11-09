package fastletproxy

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	dataplane "fast-sandbox/internal/dataplane/contract"
)

type RouteState string

const (
	RouteReady     RouteState = "Ready"
	RouteDraining  RouteState = "Draining"
	RouteSuspended RouteState = "Suspended"
)

var (
	ErrRouteNotFound = errors.New("sandbox route not found")
	ErrRouteStale    = errors.New("stale sandbox route generation")
	ErrRouteConflict = errors.New("conflicting sandbox route")
	ErrRouteDraining = errors.New("sandbox route is draining")
)

type RouteKey struct {
	SandboxUID      string `json:"sandboxUid"`
	RouteGeneration int64  `json:"routeGeneration"`
}

// RouteSpec is the mutable route value associated with a generation-fenced
// Sandbox key. Target ports are deliberately absent: one AccessDescriptor
// admits arbitrary validated ports and the signed credential narrows each
// external request.
type RouteSpec struct {
	Namespace         string                              `json:"namespace"`
	FastletPodUID     string                              `json:"fastletPodUid"`
	AssignmentAttempt int64                               `json:"assignmentAttempt"`
	Access            dataplane.AccessDescriptor          `json:"access"`
	State             RouteState                          `json:"state"`
	Components        map[string]dataplane.ComponentRoute `json:"components,omitempty"`
}

type Route struct {
	RouteKey  `json:"key"`
	RouteSpec `json:"spec"`
}

func (r Route) validate() error {
	if r.Namespace == "" || r.SandboxUID == "" || r.FastletPodUID == "" ||
		r.AssignmentAttempt <= 0 || r.RouteGeneration <= 0 || r.Access.Kind == "" {
		return fmt.Errorf("%w: incomplete route identity", ErrRouteConflict)
	}
	if err := r.Access.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrRouteConflict, err)
	}
	if r.State == "" {
		r.State = RouteReady
	}
	if r.State != RouteReady && r.State != RouteDraining && r.State != RouteSuspended {
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
	Revision uint64     `json:"revision"`
	Type     EventType  `json:"type"`
	Key      RouteKey   `json:"key"`
	Spec     *RouteSpec `json:"spec,omitempty"`
}

type Snapshot struct {
	Revision uint64  `json:"revision"`
	Routes   []Route `json:"routes"`
}

type routeAvailability struct {
	generation int64
	suspended  bool
}

// Store keeps generation tombstones after deletion. This prevents a delayed
// ApplyRoute from resurrecting an old runtime even though no active route is
// currently present. Active entries are immutable *Route values in sync.Map,
// so stable data-plane lookups do not take writeMu. The mutex only serializes
// control-plane transitions and their revision/watcher ordering.
type Store struct {
	writeMu      sync.Mutex
	active       sync.Map // Sandbox UID string -> immutable *Route
	availability map[string]routeAvailability
	highWater    map[string]int64
	revision     uint64
	nextWatcher  uint64
	watchers     map[uint64]chan Event
}

func NewStore() *Store {
	store := &Store{
		availability: make(map[string]routeAvailability), highWater: make(map[string]int64), watchers: make(map[uint64]chan Event),
	}
	return store
}

func (s *Store) Apply(route Route) (uint64, error) {
	if route.State == "" {
		route.State = RouteReady
	}
	if err := route.validate(); err != nil {
		return 0, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// An Apply already in flight when suspension began may populate the
	// route, but must not reopen it. Suspension also covers absent routes.
	if availability, ok := s.availability[route.SandboxUID]; ok {
		if route.RouteGeneration < availability.generation {
			return s.revision, ErrRouteStale
		}
		if route.RouteGeneration == availability.generation && availability.suspended {
			route.State = RouteSuspended
		}
	}
	high := s.highWater[route.SandboxUID]
	if route.RouteGeneration < high {
		return s.revision, ErrRouteStale
	}
	if existing, ok := s.loadRoute(route.SandboxUID); ok && route.RouteGeneration == existing.RouteGeneration {
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
	s.storeRouteLocked(route)
	s.revision++
	s.publishLocked(Event{Revision: s.revision, Type: EventApplied, Key: route.RouteKey, Spec: routeSpecPtr(route.RouteSpec)})
	return s.revision, nil
}

func (s *Store) MarkDraining(sandboxUID string, generation int64) (uint64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	route, ok := s.loadRoute(sandboxUID)
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
	s.storeRouteLocked(route)
	s.revision++
	s.publishLocked(Event{Revision: s.revision, Type: EventDraining, Key: route.RouteKey, Spec: routeSpecPtr(route.RouteSpec)})
	return s.revision, nil
}

func (s *Store) Delete(sandboxUID string, generation int64) (uint64, error) {
	if sandboxUID == "" || generation <= 0 {
		return 0, ErrRouteConflict
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	high := s.highWater[sandboxUID]
	if generation < high {
		return s.revision, ErrRouteStale
	}
	if generation == high {
		if _, exists := s.active.Load(sandboxUID); !exists {
			return s.revision, nil
		}
	}
	s.highWater[sandboxUID] = generation
	s.active.Delete(sandboxUID)
	s.revision++
	s.publishLocked(Event{Revision: s.revision, Type: EventDeleted, Key: RouteKey{SandboxUID: sandboxUID, RouteGeneration: generation}})
	return s.revision, nil
}

func (s *Store) Lookup(sandboxUID string) (Route, error) {
	route, ok := s.loadRoute(sandboxUID)
	if !ok {
		return Route{}, ErrRouteNotFound
	}
	if route.State == RouteDraining || route.State == RouteSuspended {
		return Route{}, ErrRouteDraining
	}
	return cloneRoute(route), nil
}

func (s *Store) Snapshot() Snapshot {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	routes := make([]Route, 0)
	s.active.Range(func(_, value any) bool {
		route, ok := value.(*Route)
		if ok && route != nil {
			routes = append(routes, cloneRoute(*route))
		}
		return true
	})
	sort.Slice(routes, func(i, j int) bool { return routes[i].SandboxUID < routes[j].SandboxUID })
	return Snapshot{Revision: s.revision, Routes: routes}
}

func (s *Store) Subscribe() (<-chan Event, func()) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.nextWatcher++
	id := s.nextWatcher
	ch := make(chan Event, 64)
	s.watchers[id] = ch
	return ch, func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
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

func (s *Store) loadRoute(sandboxUID string) (Route, bool) {
	value, exists := s.active.Load(sandboxUID)
	if !exists {
		return Route{}, false
	}
	route, valid := value.(*Route)
	if !valid || route == nil {
		return Route{}, false
	}
	return *route, true
}

func (s *Store) storeRouteLocked(route Route) {
	stored := cloneRoute(route)
	s.active.Store(route.SandboxUID, &stored)
}

func cloneRoute(route Route) Route {
	clone := route
	if route.Components != nil {
		clone.Components = make(map[string]dataplane.ComponentRoute, len(route.Components))
		for name, component := range route.Components {
			clone.Components[name] = component
		}
	}
	return clone
}

func routeSpecPtr(spec RouteSpec) *RouteSpec {
	clone := spec
	if spec.Components != nil {
		clone.Components = make(map[string]dataplane.ComponentRoute, len(spec.Components))
		for name, component := range spec.Components {
			clone.Components[name] = component
		}
	}
	return &clone
}

// SetSuspended uses optimistic control revisions so a delayed suspend/resume
// cannot override a newer transition. Delete tombstones remain irreversible.
func (s *Store) SetSuspended(uid string, generation int64, revision uint64, suspended bool) (uint64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if uid == "" || generation <= 0 || revision != s.revision {
		return s.revision, ErrRouteConflict
	}
	route, exists := s.loadRoute(uid)
	high := s.highWater[uid]
	if generation < high || generation < s.availability[uid].generation || (generation == high && high != 0 && !exists) {
		return s.revision, ErrRouteStale
	}
	if exists && route.RouteGeneration == generation && route.State == RouteDraining {
		return s.revision, ErrRouteConflict
	}
	// Retain the availability generation after resume to fence older Apply calls.
	s.availability[uid] = routeAvailability{generation: generation, suspended: suspended}
	s.revision++ // Fence late requests even when the route is not installed yet.
	if exists {
		// A newer owner must install its own full route before becoming ready.
		if suspended || route.RouteGeneration < generation {
			route.State = RouteSuspended
		} else {
			route.State = RouteReady
		}
		s.storeRouteLocked(route)
		s.publishLocked(Event{Revision: s.revision, Type: EventApplied, Key: route.RouteKey, Spec: routeSpecPtr(route.RouteSpec)})
	}
	return s.revision, nil
}
