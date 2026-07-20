package observability

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func UnaryServerInterceptor(component string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (response any, err error) {
		ctx = extractGRPC(ctx)
		ctx, span := StartServer(ctx, "grpc.server "+info.FullMethod,
			attribute.String("rpc.system", "grpc"),
			attribute.String("rpc.method", info.FullMethod),
			attribute.String("server.component", component),
		)
		defer func() {
			if err != nil {
				span.SetAttributes(attribute.String("rpc.grpc.status_code", status.Code(err).String()))
			}
			End(span, err)
		}()
		return handler(ctx, request)
	}
}

func UnaryClientInterceptor(component string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, request, response any, connection *grpc.ClientConn, invoker grpc.UnaryInvoker, options ...grpc.CallOption) (err error) {
		ctx, span := StartClient(ctx, "grpc.client "+method,
			attribute.String("rpc.system", "grpc"),
			attribute.String("rpc.method", method),
			attribute.String("client.component", component),
		)
		defer func() { End(span, err) }()
		ctx = injectGRPC(ctx)
		return invoker(ctx, method, request, response, connection, options...)
	}
}

func extractGRPC(ctx context.Context) context.Context {
	metadataValue, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	return wirePropagator.Extract(ctx, metadataCarrier(metadataValue))
}

func injectGRPC(ctx context.Context) context.Context {
	metadataValue, _ := metadata.FromOutgoingContext(ctx)
	metadataValue = metadataValue.Copy()
	wirePropagator.Inject(ctx, metadataCarrier(metadataValue))
	return metadata.NewOutgoingContext(ctx, metadataValue)
}

type metadataCarrier metadata.MD

var _ propagation.TextMapCarrier = metadataCarrier{}

func (carrier metadataCarrier) Get(key string) string {
	values := metadata.MD(carrier).Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (carrier metadataCarrier) Set(key, value string) {
	metadata.MD(carrier).Set(key, value)
}

func (carrier metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(carrier))
	for key := range carrier {
		keys = append(keys, key)
	}
	return keys
}
