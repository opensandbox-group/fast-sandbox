package observability

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestStartHTTPServerContinuesW3CTraceAndRecordsIdentity(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	request := httptest.NewRequest("POST", "http://fastlet/api/v2/fastlet/ensure", nil)
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	request, span := StartHTTPServer(request, "fastlet")
	request = request.WithContext(WithIdentity(request.Context(), Identity{
		RequestID: "request-a", SandboxUID: "uid-a", AssignmentAttempt: 2, RouteGeneration: 3,
	}))
	InjectHTTP(request.Context(), request.Header)
	span.End()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, oteltrace.SpanKindServer, ended[0].SpanKind())
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", ended[0].SpanContext().TraceID().String())
	require.Equal(t, "00f067aa0ba902b7", ended[0].Parent().SpanID().String())
	attributes := map[string]any{}
	for _, item := range ended[0].Attributes() {
		attributes[string(item.Key)] = item.Value.AsInterface()
	}
	require.Equal(t, "request-a", attributes["fast_sandbox.request_id"])
	require.Equal(t, "uid-a", attributes["fast_sandbox.sandbox_uid"])
	require.Equal(t, int64(2), attributes["fast_sandbox.assignment_attempt"])
	require.Equal(t, int64(3), attributes["fast_sandbox.route_generation"])
	require.NotEmpty(t, request.Header.Get("traceparent"))
}
