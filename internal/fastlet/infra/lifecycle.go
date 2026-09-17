package infra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	infracatalog "fast-sandbox/internal/catalog/infra"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"k8s.io/klog/v2"
)

type TargetDialer func(context.Context, uint32) (net.Conn, error)

const (
	initialReadinessRetry = time.Millisecond
	maxReadinessRetry     = 10 * time.Millisecond
)

// InitializeInstance executes per-instance initialization and local probes.
// It dials the Sandbox private IP directly and never traverses Sandbox Proxy.
func (m *Manager) InitializeInstance(ctx context.Context, config *fastletapi.RuntimeSandboxConfig, privateIP string) (PreparedInstance, error) {
	if config == nil || privateIP == "" {
		return PreparedInstance{}, errors.New("Sandbox spec and private IP are required for Infra initialization")
	}
	return m.InitializeInstanceWithDialer(ctx, config, func(ctx context.Context, port uint32) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(privateIP, strconv.Itoa(int(port))))
	})
}

// InitializeInstanceWithDialer supports runtimes such as BoxLite whose guest
// loopback is reached through a runtime-specific LocalForward transport.
func (m *Manager) InitializeInstanceWithDialer(ctx context.Context, config *fastletapi.RuntimeSandboxConfig, dial TargetDialer) (PreparedInstance, error) {
	if config == nil || dial == nil {
		return PreparedInstance{}, errors.New("Sandbox spec and target dialer are required for Infra initialization")
	}
	instance, err := m.RecoverInstance(ctx, config)
	if err != nil {
		// A newly-created minimal profile intentionally has no instance file.
		plan, planErr := m.Plan()
		if planErr == nil && len(plan.Components) == 0 {
			return PreparedInstance{SandboxUID: config.Identity.SandboxUID}, nil
		}
		return PreparedInstance{}, fmt.Errorf("load Infra instance state: %w", err)
	}
	instance.Diagnostics = nil
	for _, service := range instance.Services {
		started := time.Now()
		var serviceErr error
		if service.HostProcess {
			// The process runs in the Fastlet Pod network namespace, so its
			// listener is reached on Pod loopback instead of the Sandbox
			// access address.
			klog.V(2).InfoS("probing host-process component on Pod loopback",
				"component", service.Component, "port", service.Port, "probe", service.Readiness.Type)
			serviceErr = m.initializeServiceWithDialer(ctx, func(ctx context.Context, port uint32) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
			}, service)
			if serviceErr != nil {
				klog.ErrorS(serviceErr, "Host-process component readiness probe failed on Pod loopback",
					"component", service.Component, "port", service.Port, "sandboxID", config.Identity.SandboxUID)
			}
		} else {
			serviceErr = m.initializeServiceWithDialer(ctx, dial, service)
		}
		m.observeInfraReady(service.Component, started, serviceErr)
		if serviceErr == nil {
			instance.Diagnostics = append(instance.Diagnostics, ComponentDiagnostic{
				Component: service.Component, State: "Ready",
			})
			continue
		}
		instance.Diagnostics = append(instance.Diagnostics, ComponentDiagnostic{
			Component: service.Component, State: "Failed", Message: serviceErr.Error(),
		})
		return instance, fmt.Errorf("component %s: %w", service.Component, serviceErr)
	}
	return instance, nil
}

func (m *Manager) initializeServiceWithDialer(ctx context.Context, dial TargetDialer, service ServiceEndpoint) error {
	client, transport := serviceHTTPClient(dial, service.Port)
	defer transport.CloseIdleConnections()
	return probeServiceWithDialer(ctx, service.Port, service.Readiness, dial, client)
}

func probeService(ctx context.Context, address string, probe infracatalog.ReadinessProbe) error {
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
	return probeServiceWithDialer(ctx, uint32(port), probe, dial, client)
}

func probeServiceWithDialer(ctx context.Context, port uint32, probe infracatalog.ReadinessProbe, dial TargetDialer, client *http.Client) error {
	address := net.JoinHostPort("sandbox.local", strconv.Itoa(int(port)))
	timeout := probe.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	retryCeiling := maxReadinessRetry
	retryDelay := min(initialReadinessRetry, retryCeiling)
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
		timer := time.NewTimer(retryDelay)
		select {
		case <-probeContext.Done():
			timer.Stop()
			return errors.Join(probeContext.Err(), lastErr)
		case <-timer.C:
		}
		retryDelay = min(retryDelay*2, retryCeiling)
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
