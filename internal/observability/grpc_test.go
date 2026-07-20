package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

func TestGRPCPropagationPreservesTraceContext(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	ctx, span := Start(context.Background(), "client")
	outgoing := injectGRPC(ctx)
	values, ok := metadata.FromOutgoingContext(outgoing)
	require.True(t, ok)
	require.NotEmpty(t, values.Get("traceparent"))

	incoming := metadata.NewIncomingContext(context.Background(), values)
	extracted := extractGRPC(incoming)
	require.Equal(t, span.SpanContext().TraceID(), oteltrace.SpanContextFromContext(extracted).TraceID())
	span.End()
}
