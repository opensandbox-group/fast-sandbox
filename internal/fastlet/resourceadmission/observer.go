package resourceadmission

import (
	"context"
	"errors"
	"time"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type Sample struct {
	MemoryCurrentBytes uint64
	MemoryLimitBytes   uint64
	OOMKills           uint64
	MemoryPSIAvg10     float64
	NodeAvailableRatio float64
}

type Reader interface {
	Read() (Sample, error)
}

type StateSetter interface {
	SetResourceAdmissionState(fastletapi.AdmissionState, fastletapi.AdmissionReason) error
}

type Config struct {
	Interval              time.Duration
	HighWatermark         float64
	LowWatermark          float64
	HighSamples           int
	RecoverySamples       int
	PSIAvg10Threshold     float64
	NodeLowWatermark      float64
	NodeRecoveryWatermark float64
}

func DefaultConfig() Config {
	return Config{
		Interval: 2 * time.Second, HighWatermark: 0.90, LowWatermark: 0.80,
		HighSamples: 3, RecoverySamples: 5, PSIAvg10Threshold: 10,
		NodeLowWatermark: 0.05, NodeRecoveryWatermark: 0.10,
	}
}

func (c Config) Validate() error {
	if c.Interval <= 0 || c.HighWatermark <= 0 || c.HighWatermark > 1 || c.LowWatermark < 0 || c.LowWatermark >= c.HighWatermark {
		return errors.New("invalid memory observer interval or watermarks")
	}
	if c.HighSamples <= 0 || c.RecoverySamples <= 0 || c.PSIAvg10Threshold < 0 {
		return errors.New("invalid memory observer samples or PSI threshold")
	}
	if c.NodeLowWatermark < 0 || c.NodeRecoveryWatermark < c.NodeLowWatermark || c.NodeRecoveryWatermark > 1 {
		return errors.New("invalid node memory watermarks")
	}
	return nil
}

type Observer struct {
	reader Reader
	setter StateSetter
	config Config

	state           fastletapi.AdmissionState
	reason          fastletapi.AdmissionReason
	highSamples     int
	recoverySamples int
	lastOOMKills    uint64
	initialized     bool
}

func NewObserver(reader Reader, setter StateSetter, config Config) (*Observer, error) {
	if reader == nil || setter == nil {
		return nil, errors.New("memory observer reader and state setter are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Observer{reader: reader, setter: setter, config: config, state: fastletapi.AdmissionStateUnknown, reason: fastletapi.AdmissionReasonTelemetryMissing}, nil
}

func (o *Observer) Run(ctx context.Context) {
	_ = o.setter.SetResourceAdmissionState(fastletapi.AdmissionStateUnknown, fastletapi.AdmissionReasonTelemetryMissing)
	o.observe()
	ticker := time.NewTicker(o.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.observe()
		}
	}
}

func (o *Observer) ObserveOnce() (fastletapi.AdmissionState, fastletapi.AdmissionReason) {
	o.observe()
	return o.state, o.reason
}

func (o *Observer) observe() {
	sample, err := o.reader.Read()
	if err != nil || sample.MemoryLimitBytes == 0 {
		o.transition(fastletapi.AdmissionStateUnknown, fastletapi.AdmissionReasonTelemetryMissing)
		o.highSamples = 0
		o.recoverySamples = 0
		return
	}

	firstValidSample := !o.initialized
	oomIncreased := o.initialized && sample.OOMKills > o.lastOOMKills
	o.initialized = true
	o.lastOOMKills = sample.OOMKills
	usageRatio := float64(sample.MemoryCurrentBytes) / float64(sample.MemoryLimitBytes)

	switch {
	case oomIncreased:
		o.highSamples = 0
		o.recoverySamples = 0
		o.transition(fastletapi.AdmissionStateBlocked, fastletapi.AdmissionReasonFastletOOMEvent)
		return
	case o.config.NodeLowWatermark > 0 && sample.NodeAvailableRatio > 0 && sample.NodeAvailableRatio <= o.config.NodeLowWatermark:
		o.highSamples = 0
		o.recoverySamples = 0
		o.transition(fastletapi.AdmissionStateBlocked, fastletapi.AdmissionReasonNodeMemoryPressure)
		return
	case o.config.PSIAvg10Threshold > 0 && sample.MemoryPSIAvg10 >= o.config.PSIAvg10Threshold:
		o.highSamples = 0
		o.recoverySamples = 0
		o.transition(fastletapi.AdmissionStateBlocked, fastletapi.AdmissionReasonFastletMemoryPSI)
		return
	case usageRatio >= o.config.HighWatermark:
		o.highSamples++
		o.recoverySamples = 0
		if o.highSamples >= o.config.HighSamples {
			o.transition(fastletapi.AdmissionStateBlocked, fastletapi.AdmissionReasonFastletMemoryHighWatermark)
		}
		return
	}

	o.highSamples = 0
	if usageRatio <= o.config.LowWatermark &&
		(o.config.NodeRecoveryWatermark == 0 || sample.NodeAvailableRatio == 0 || sample.NodeAvailableRatio >= o.config.NodeRecoveryWatermark) {
		o.recoverySamples++
		if firstValidSample || o.recoverySamples >= o.config.RecoverySamples {
			o.transition(fastletapi.AdmissionStateOpen, "")
		}
		return
	}
	o.recoverySamples = 0
}

func (o *Observer) transition(state fastletapi.AdmissionState, reason fastletapi.AdmissionReason) {
	if o.state == state && o.reason == reason {
		return
	}
	if err := o.setter.SetResourceAdmissionState(state, reason); err != nil {
		return
	}
	o.state = state
	o.reason = reason
}
