package resourceadmission

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type sequenceReader struct {
	samples []Sample
	errs    []error
	index   int
}

func (r *sequenceReader) Read() (Sample, error) {
	index := r.index
	if index >= len(r.samples) {
		index = len(r.samples) - 1
	}
	r.index++
	if index >= 0 && index < len(r.errs) && r.errs[index] != nil {
		return Sample{}, r.errs[index]
	}
	return r.samples[index], nil
}

type recordingSetter struct {
	states  []fastletapi.AdmissionState
	reasons []fastletapi.AdmissionReason
}

func (s *recordingSetter) SetResourceAdmissionState(state fastletapi.AdmissionState, reason fastletapi.AdmissionReason) error {
	s.states = append(s.states, state)
	s.reasons = append(s.reasons, reason)
	return nil
}

func testConfig() Config {
	config := DefaultConfig()
	config.Interval = time.Millisecond
	config.HighSamples = 2
	config.RecoverySamples = 2
	return config
}

func TestObserverAppliesHighLowWatermarkHysteresis(t *testing.T) {
	reader := &sequenceReader{samples: []Sample{
		{MemoryCurrentBytes: 50, MemoryLimitBytes: 100, NodeAvailableRatio: 0.5},
		{MemoryCurrentBytes: 91, MemoryLimitBytes: 100, NodeAvailableRatio: 0.5},
		{MemoryCurrentBytes: 92, MemoryLimitBytes: 100, NodeAvailableRatio: 0.5},
		{MemoryCurrentBytes: 85, MemoryLimitBytes: 100, NodeAvailableRatio: 0.5},
		{MemoryCurrentBytes: 79, MemoryLimitBytes: 100, NodeAvailableRatio: 0.5},
		{MemoryCurrentBytes: 78, MemoryLimitBytes: 100, NodeAvailableRatio: 0.5},
	}}
	setter := &recordingSetter{}
	observer, err := NewObserver(reader, setter, testConfig())
	require.NoError(t, err)

	state, _ := observer.ObserveOnce()
	require.Equal(t, fastletapi.AdmissionStateOpen, state)
	observer.ObserveOnce()
	state, reason := observer.ObserveOnce()
	require.Equal(t, fastletapi.AdmissionStateBlocked, state)
	require.Equal(t, fastletapi.AdmissionReasonFastletMemoryHighWatermark, reason)
	state, _ = observer.ObserveOnce()
	require.Equal(t, fastletapi.AdmissionStateBlocked, state, "the gap between watermarks preserves the block")
	observer.ObserveOnce()
	state, reason = observer.ObserveOnce()
	require.Equal(t, fastletapi.AdmissionStateOpen, state)
	require.Empty(t, reason)
}

func TestObserverBlocksOOMPSIAndNodePressure(t *testing.T) {
	tests := []struct {
		name    string
		samples []Sample
		reason  fastletapi.AdmissionReason
	}{
		{name: "oom increase", samples: []Sample{
			{MemoryCurrentBytes: 50, MemoryLimitBytes: 100, OOMKills: 1, NodeAvailableRatio: 0.5},
			{MemoryCurrentBytes: 50, MemoryLimitBytes: 100, OOMKills: 2, NodeAvailableRatio: 0.5},
		}, reason: fastletapi.AdmissionReasonFastletOOMEvent},
		{name: "memory PSI", samples: []Sample{{MemoryCurrentBytes: 50, MemoryLimitBytes: 100, MemoryPSIAvg10: 11, NodeAvailableRatio: 0.5}}, reason: fastletapi.AdmissionReasonFastletMemoryPSI},
		{name: "node memory", samples: []Sample{{MemoryCurrentBytes: 50, MemoryLimitBytes: 100, NodeAvailableRatio: 0.04}}, reason: fastletapi.AdmissionReasonNodeMemoryPressure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observer, err := NewObserver(&sequenceReader{samples: test.samples}, &recordingSetter{}, testConfig())
			require.NoError(t, err)
			var state fastletapi.AdmissionState
			var reason fastletapi.AdmissionReason
			for range test.samples {
				state, reason = observer.ObserveOnce()
			}
			require.Equal(t, fastletapi.AdmissionStateBlocked, state)
			require.Equal(t, test.reason, reason)
		})
	}
}

func TestObserverFailsClosedWhenTelemetryDisappears(t *testing.T) {
	reader := &sequenceReader{
		samples: []Sample{{MemoryCurrentBytes: 50, MemoryLimitBytes: 100, NodeAvailableRatio: 0.5}, {}},
		errs:    []error{nil, errors.New("cgroup removed")},
	}
	observer, err := NewObserver(reader, &recordingSetter{}, testConfig())
	require.NoError(t, err)
	observer.ObserveOnce()
	state, reason := observer.ObserveOnce()
	require.Equal(t, fastletapi.AdmissionStateUnknown, state)
	require.Equal(t, fastletapi.AdmissionReasonTelemetryMissing, reason)
}
