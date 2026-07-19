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

type TargetDialer func(context.Context, uint32) (net.Conn, error)

// InitializeInstance executes per-instance initialization and local probes.
// It dials the Sandbox private IP directly and never traverses Sandbox Proxy.
func (m *Manager) InitializeInstance(ctx context.Context, spec *api.SandboxSpec, privateIP string) (PreparedInstance, error) {
	if spec == nil || privateIP == "" {
		return PreparedInstance{}, errors.New("Sandbox spec and private IP are required for Infra initialization")
	}
	return m.InitializeInstanceWithDialer(ctx, spec, func(ctx context.Context, port uint32) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(privateIP, strconv.Itoa(int(port))))
	})
}

// InitializeInstanceWithDialer supports runtimes such as BoxLite whose guest
// loopback is reached through a runtime-specific LocalForward transport.
func (m *Manager) InitializeInstanceWithDialer(ctx context.Context, spec *api.SandboxSpec, dial TargetDialer) (PreparedInstance, error) {
	if spec == nil || dial == nil {
		return PreparedInstance{}, errors.New("Sandbox spec and target dialer are required for Infra initialization")
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
		serviceErr := m.initializeServiceWithDialer(ctx, spec, dial, service, instance.UpstreamHeaders)
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
	return m.initializeServiceWithDialer(ctx, spec, func(ctx context.Context, port uint32) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(privateIP, strconv.Itoa(int(port))))
	}, service, headers)
}

func (m *Manager) initializeServiceWithDialer(ctx context.Context, spec *api.SandboxSpec, dial TargetDialer, service ServiceEndpoint, headers map[string]string) error {
	client, transport := serviceHTTPClient(dial, service.Port)
	defer transport.CloseIdleConnections()
	address := net.JoinHostPort("sandbox.local", strconv.Itoa(int(service.Port)))
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
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("instance init: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("instance init returned HTTP %d", response.StatusCode)
		}
	}
	return probeServiceWithDialer(ctx, service.Port, service.Readiness, headers, dial, client)
}

func probeService(ctx context.Context, address string, probe infracatalog.ReadinessProbe, headers map[string]string) error {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return errors.New("service address has an invalid port")
	}
	dial := func(ctx context.Context, _ uint32) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	client, transport := serviceHTTPClient(dial, uint32(port))
	defer transport.CloseIdleConnections()
	return probeServiceWithDialer(ctx, uint32(port), probe, headers, dial, client)
}

func probeServiceWithDialer(ctx context.Context, port uint32, probe infracatalog.ReadinessProbe, headers map[string]string, dial TargetDialer, client *http.Client) error {
	if probe.Type == "" || probe.Type == infracatalog.ProbeNone {
		return nil
	}
	address := net.JoinHostPort("sandbox.local", strconv.Itoa(int(port)))
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
			response, err := client.Do(request)
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
			connection, err := dial(probeContext, port)
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

func serviceHTTPClient(dial TargetDialer, port uint32) (*http.Client, *http.Transport) {
	transport := &http.Transport{
		Proxy: nil, DisableCompression: true, ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, port)
		},
	}
	return &http.Client{Transport: transport}, transport
}
