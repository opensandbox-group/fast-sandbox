package testutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManualClock(t *testing.T) {
	start := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	clock := NewManualClock(start)
	require.Equal(t, start, clock.Now())
	require.Equal(t, start.Add(5*time.Second), clock.Advance(5*time.Second))
	require.Equal(t, start.Add(5*time.Second), clock.Now())
}
