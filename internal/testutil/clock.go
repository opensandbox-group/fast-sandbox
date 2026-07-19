// Package testutil contains deterministic fakes shared by control-plane and
// Fastlet state-machine tests. It must not be imported by production packages.
package testutil

import (
	"sync"
	"time"
)

// ManualClock is a concurrency-safe clock controlled by tests.
type ManualClock struct {
	mu  sync.RWMutex
	now time.Time
}

func NewManualClock(now time.Time) *ManualClock {
	return &ManualClock{now: now}
}

func (c *ManualClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *ManualClock) Advance(duration time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
	return c.now
}
