package observability

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collectortracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

type traceReceiver struct {
	collectortracev1.UnimplementedTraceServiceServer
	requests chan *collectortracev1.ExportTraceServiceRequest
}

func (r *traceReceiver) Export(_ context.Context, request *collectortracev1.ExportTraceServiceRequest) (*collectortracev1.ExportTraceServiceResponse, error) {
	r.requests <- request
	return &collectortracev1.ExportTraceServiceResponse{}, nil
}

func TestConfigureLeavesTracingNoOpWithoutExplicitEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	shutdown, err := Configure(context.Background(), "fast-sandbox-test")
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
}

func TestTracingDisabledOverridesEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	require.False(t, tracingEnabled())
}

func TestConfigureExportsAndFlushesOTLPTrace(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	receiver := &traceReceiver{requests: make(chan *collectortracev1.ExportTraceServiceRequest, 1)}
	server := grpc.NewServer()
	collectortracev1.RegisterTraceServiceServer(server, receiver)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_SERVICE_NAME", "fast-sandbox-otlp-test")
	previous := otel.GetTracerProvider()
	shutdown, err := Configure(context.Background(), "unused-default")
	require.NoError(t, err)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	_, span := Start(context.Background(), "otlp-smoke")
	span.End()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, shutdown(shutdownContext))

	select {
	case request := <-receiver.requests:
		require.NotEmpty(t, request.ResourceSpans)
		resourceSpans := request.ResourceSpans[0]
		serviceName := ""
		for _, item := range resourceSpans.Resource.Attributes {
			if item.Key == "service.name" {
				serviceName = item.Value.GetStringValue()
			}
		}
		require.Equal(t, "fast-sandbox-otlp-test", serviceName)
		require.NotEmpty(t, resourceSpans.ScopeSpans)
		require.NotEmpty(t, resourceSpans.ScopeSpans[0].Spans)
		require.Equal(t, "otlp-smoke", resourceSpans.ScopeSpans[0].Spans[0].Name)
	case <-time.After(5 * time.Second):
		t.Fatal("OTLP collector did not receive the flushed span")
	}
}
