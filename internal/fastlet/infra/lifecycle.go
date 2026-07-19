package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/infracatalog"
)

type initPayload struct {
	SandboxUID         string            `json:"sandboxUid"`
	InstanceGeneration int64             `json:"instanceGeneration"`
	AssignmentAttempt  int64             `json:"assignmentAttempt"`
	Environment        map[string]string `json:"environment,omitempty"`
}

// InitializeInstance executes per-instance initialization and local probes.
// It dials the Sandbox private IP directly and never traverses Sandbox Proxy.
func (m *Manager) InitializeInstance(ctx context.Context, spec *api.SandboxSpec, privateIP string) (PreparedInstance, error) {
	if spec == nil || privateIP == "" {
		return PreparedInstance{}, errors.New("Sandbox spec and private IP are required for Infra initialization")
	}
	instance, err := m.RecoverInstance(ctx, spec)
	if err != nil {
		// A newly-created minimal profile intentionally has no instance file.
		plan, planErr := m.Plan()
		if planErr == nil && len(plan.Components) == 0 {
			return PreparedInstance{SandboxUID: spec.SandboxID}, nil
		}
		return PreparedInstance{}, fmt.Errorf("load Infra instance state: %w", err)
	}
	instance.Diagnostics = nil
	for _, service := range instance.Services {
		serviceErr := m.initializeService(ctx, spec, privateIP, service, instance.UpstreamHeaders)
		if serviceErr == nil {
			instance.Diagnostics = append(instance.Diagnostics, ComponentDiagnostic{
				Component: service.Component, Service: service.Name, Required: service.Required, State: "Ready",
			})
			continue
		}
		wrapped := fmt.Errorf("component %s service %s: %w", service.Component, service.Name, serviceErr)
		instance.Diagnostics = append(instance.Diagnostics, ComponentDiagnostic{
			Component: service.Component, Service: service.Name, Required: service.Required, State: "Failed", Message: serviceErr.Error(),
		})
		if service.Required {
			return instance, wrapped
		}
	}
	// Optional failures are intentionally diagnostic-only and do not gate the
	// Sandbox. The caller can expose them without suppressing route publication.
	return instance, nil
}

func (m *Manager) initializeService(ctx context.Context, spec *api.SandboxSpec, privateIP string, service ServiceEndpoint, headers map[string]string) error {
	address := net.JoinHostPort(privateIP, strconv.Itoa(int(service.Port)))
	if service.Init.Mode == infracatalog.InitHTTP {
		payload, err := json.Marshal(initPayload{
			SandboxUID: spec.SandboxID, InstanceGeneration: spec.InstanceGeneration,
			AssignmentAttempt: spec.AssignmentAttempt, Environment: spec.Env,
		})
		if err != nil {
			return err
		}
		method := service.Init.Method
		if method == "" {
			method = http.MethodPost
		}
		request, err := http.NewRequestWithContext(ctx, method, "http://"+address+service.Init.Path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := localHTTPClient().Do(request)
		if err != nil {
			return fmt.Errorf("instance init: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("instance init returned HTTP %d", response.StatusCode)
		}
	}
	return probeService(ctx, address, service.Readiness, headers)
}

func probeService(ctx context.Context, address string, probe infracatalog.ReadinessProbe, headers map[string]string) error {
	if probe.Type == "" || probe.Type == infracatalog.ProbeNone {
		return nil
	}
	timeout := probe.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	interval := probe.Interval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	probeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		switch probe.Type {
		case infracatalog.ProbeHTTP:
			request, err := http.NewRequestWithContext(probeContext, http.MethodGet, "http://"+address+probe.Path, nil)
			if err != nil {
				return err
			}
			for name, value := range headers {
				request.Header.Set(name, value)
			}
			response, err := localHTTPClient().Do(request)
			lastErr = err
			if response != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
				_ = response.Body.Close()
				if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusBadRequest {
					return nil
				}
				lastErr = fmt.Errorf("readiness returned HTTP %d", response.StatusCode)
			}
		case infracatalog.ProbeTCP:
			connection, err := (&net.Dialer{}).DialContext(probeContext, "tcp", address)
			lastErr = err
			if connection != nil {
				_ = connection.Close()
				return nil
			}
		default:
			return fmt.Errorf("unsupported readiness probe %s", probe.Type)
		}
		timer := time.NewTimer(interval)
		select {
		case <-probeContext.Done():
			timer.Stop()
			return errors.Join(probeContext.Err(), lastErr)
		case <-timer.C:
		}
	}
}

func localHTTPClient() *http.Client {
	return infraHTTPClient
}

var infraHTTPClient = &http.Client{Transport: &http.Transport{Proxy: nil, DisableCompression: true}}
