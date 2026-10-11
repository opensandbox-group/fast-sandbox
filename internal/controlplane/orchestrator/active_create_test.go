package orchestrator

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestActiveCreateTrackerIsScopedAndReferenceCounted(t *testing.T) {
	o := &Orchestrator{}
	key := types.NamespacedName{Namespace: "default", Name: "sandbox-a"}
	first := o.BeginFastPathCreate(key)
	second := o.BeginFastPathCreate(key)
	require.True(t, o.FastPathCreateActive(key))
	require.False(t, o.FastPathCreateActive(types.NamespacedName{Namespace: "other", Name: key.Name}))
	first()
	first()
	require.True(t, o.FastPathCreateActive(key))
	second()
	require.False(t, o.FastPathCreateActive(key))
	require.Empty(t, o.activeCreates)
}

func TestActiveCreateTrackerConcurrentRelease(t *testing.T) {
	o := &Orchestrator{}
	key := types.NamespacedName{Namespace: "default", Name: "sandbox-a"}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := o.BeginFastPathCreate(key)
			_ = o.FastPathCreateActive(key)
			release()
		}()
	}
	wg.Wait()
	require.False(t, o.FastPathCreateActive(key))
	require.Empty(t, o.activeCreates)
}
