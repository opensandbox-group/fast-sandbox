package observability

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/klog/v2"
)

const instrumentationName = "fast-sandbox"

var wirePropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
)

type identityContextKey struct{}

// Identity contains high-cardinality lifecycle fences that belong in traces
// and structured logs, never Prometheus labels.
type Identity struct {
	RequestID          string
	Namespace          string
	SandboxName        string
	SandboxUID         string
	FastletPodUID      string
	InstanceGeneration int64
	AssignmentAttempt  int64
	RouteGeneration    int64
	TargetPort         uint32
}

// Start creates a span through the process-global OpenTelemetry provider. The
// default provider is a no-op, allowing deployments to install an SDK/exporter
// without coupling core packages to one collector implementation.
func Start(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	return start(ctx, name, trace.SpanKindInternal, attributes...)
}

func StartClient(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	return start(ctx, name, trace.SpanKindClient, attributes...)
}

func StartServer(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	return start(ctx, name, trace.SpanKindServer, attributes...)
}

func start(ctx context.Context, name string, kind trace.SpanKind, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	if identity, ok := ctx.Value(identityContextKey{}).(Identity); ok {
		identityAttributes, _ := identityFields(identity)
		attributes = append(attributes, identityAttributes...)
	}
	return otel.Tracer(instrumentationName).Start(ctx, name,
		trace.WithSpanKind(kind),
		trace.WithAttributes(attributes...),
	)
}

func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// WithIdentity enriches both the active span and the logger stored in context.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	update := identity
	if existing, ok := ctx.Value(identityContextKey{}).(Identity); ok {
		identity = mergeIdentity(existing, identity)
	}
	attributes, _ := identityFields(identity)
	if len(attributes) > 0 {
		trace.SpanFromContext(ctx).SetAttributes(attributes...)
	}
	ctx = context.WithValue(ctx, identityContextKey{}, identity)
	_, values := identityFields(update)
	if len(values) > 0 {
		ctx = klog.NewContext(ctx, klog.FromContext(ctx).WithValues(values...))
	}
	return ctx
}

func ExtractHTTP(ctx context.Context, header http.Header) context.Context {
	return wirePropagator.Extract(ctx, propagation.HeaderCarrier(header))
}

func InjectHTTP(ctx context.Context, header http.Header) {
	wirePropagator.Inject(ctx, propagation.HeaderCarrier(header))
}

// StartHTTPServer extracts an upstream W3C context before starting a server span.
func StartHTTPServer(request *http.Request, component string) (*http.Request, trace.Span) {
	ctx := ExtractHTTP(request.Context(), request.Header)
	ctx, span := StartServer(ctx, component+" "+request.Method,
		attribute.String("http.request.method", request.Method),
		attribute.String("url.path", request.URL.Path),
		attribute.String("server.component", component),
	)
	return request.WithContext(ctx), span
}

func identityFields(identity Identity) ([]attribute.KeyValue, []any) {
	attributes := make([]attribute.KeyValue, 0, 9)
	values := make([]any, 0, 18)
	addString := func(key string, value string) {
		if value == "" {
			return
		}
		attributes = append(attributes, attribute.String("fast_sandbox."+key, value))
		values = append(values, key, value)
	}
	addInt64 := func(key string, value int64) {
		if value <= 0 {
			return
		}
		attributes = append(attributes, attribute.Int64("fast_sandbox."+key, value))
		values = append(values, key, value)
	}
	addString("request_id", identity.RequestID)
	addString("namespace", identity.Namespace)
	addString("sandbox_name", identity.SandboxName)
	addString("sandbox_uid", identity.SandboxUID)
	addString("fastlet_pod_uid", identity.FastletPodUID)
	addInt64("instance_generation", identity.InstanceGeneration)
	addInt64("assignment_attempt", identity.AssignmentAttempt)
	addInt64("route_generation", identity.RouteGeneration)
	if identity.TargetPort > 0 {
		attributes = append(attributes, attribute.Int64("fast_sandbox.target_port", int64(identity.TargetPort)))
		values = append(values, "target_port", identity.TargetPort)
	}
	return attributes, values
}

func mergeIdentity(existing, update Identity) Identity {
	if update.RequestID == "" {
		update.RequestID = existing.RequestID
	}
	if update.Namespace == "" {
		update.Namespace = existing.Namespace
	}
	if update.SandboxName == "" {
		update.SandboxName = existing.SandboxName
	}
	if update.SandboxUID == "" {
		update.SandboxUID = existing.SandboxUID
	}
	if update.FastletPodUID == "" {
		update.FastletPodUID = existing.FastletPodUID
	}
	if update.InstanceGeneration <= 0 {
		update.InstanceGeneration = existing.InstanceGeneration
	}
	if update.AssignmentAttempt <= 0 {
		update.AssignmentAttempt = existing.AssignmentAttempt
	}
	if update.RouteGeneration <= 0 {
		update.RouteGeneration = existing.RouteGeneration
	}
	if update.TargetPort == 0 {
		update.TargetPort = existing.TargetPort
	}
	return update
}
