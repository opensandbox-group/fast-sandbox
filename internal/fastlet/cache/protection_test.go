package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProtectionIndexNeverEvictsWarmActiveInfraOrHotContent(t *testing.T) {
	now := time.Unix(100, 0)
	index := NewProtectionIndex(func() time.Time { return now })
	index.Protect("docker.io/library/warm:1", ProtectWarm)
	index.Protect("active:1", ProtectActive)
	index.Protect("infra:1", ProtectInfra)
	index.ProtectHotUntil("hot:1", now.Add(time.Minute))

	require.Equal(t, []string{"cold:1"}, index.PlanEviction([]string{"warm:1", "active:1", "infra:1", "hot:1", "cold:1"}))
	now = now.Add(2 * time.Minute)
	require.Equal(t, []string{"cold:1", "hot:1"}, index.PlanEviction([]string{"warm:1", "active:1", "infra:1", "hot:1", "cold:1"}))

	index.Unprotect("warm:1", ProtectWarm)
	index.Unprotect("active:1", ProtectActive)
	index.Unprotect("infra:1", ProtectInfra)
	require.Equal(t, []string{"active:1", "cold:1", "hot:1", "infra:1", "warm:1"}, index.PlanEviction([]string{"warm:1", "active:1", "infra:1", "hot:1", "cold:1"}))
}
