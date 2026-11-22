package fastpath

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "fast-sandbox/api/proto/v2"
	"fast-sandbox/internal/fastletauth/fastletauthtest"
)

const serverSubject = "system:serviceaccount:identities:server-caller"
const gatewaySubject = "system:serviceaccount:identities:gateway-caller"

type authenticatedPoolService struct {
	pb.UnimplementedFastPathServiceServer
}

func (authenticatedPoolService) GetPool(_ context.Context, req *pb.GetPoolRequest) (*pb.PoolInfo, error) {
	return &pb.PoolInfo{Name: req.PoolName}, nil
}

func TestCallerAuthenticationBothTransports(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	auth, err := NewCallerAuthenticator(issuer.URL(), []CallerGrant{
		{Subject: serverSubject, Profile: "server", Namespaces: []string{"tenant-a"}},
		{Subject: gatewaySubject, Profile: "gateway", Namespaces: []string{"tenant-a"}},
	}, &http.Client{Timeout: time.Second})
	require.NoError(t, err)
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer certificateServer.Close()
	backend := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: certificateServer.TLS.Certificates})), grpc.UnaryInterceptor(auth.UnaryInterceptor))
	pb.RegisterFastPathServiceServer(backend, authenticatedPoolService{})
	defer backend.Stop()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = backend.Serve(listener) }()
	roots := x509.NewCertPool()
	roots.AddCert(certificateServer.Certificate())
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots})))
	require.NoError(t, err)
	defer conn.Close()
	web := httptest.NewTLSServer(grpcweb.WrapServer(backend))
	defer web.Close()
	now := time.Now().Unix()
	valid := issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject, Audience: CallerAudience})
	for _, tc := range []struct {
		name, token, namespace string
		code                   codes.Code
	}{
		{"valid", valid, "tenant-a", codes.OK},
		{"anonymous", "", "tenant-a", codes.Unauthenticated},
		{"malformed", "not-a-jwt", "tenant-a", codes.Unauthenticated},
		{"wrong namespace", valid, "tenant-b", codes.PermissionDenied},
		{"empty namespace", valid, "", codes.PermissionDenied},
		{"gateway cannot read pools", issuer.Mint(fastletauthtest.MintOptions{Subject: gatewaySubject, Audience: CallerAudience}), "tenant-a", codes.PermissionDenied},
		{"fastlet audience", issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject}), "tenant-a", codes.Unauthenticated},
		{"unknown subject", issuer.Mint(fastletauthtest.MintOptions{Audience: CallerAudience}), "tenant-a", codes.PermissionDenied},
		{"overlong lifetime", issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject, Audience: CallerAudience, IssuedAt: now, ExpiresAt: now + 3601}), "tenant-a", codes.Unauthenticated},
		{"expired without leeway", issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject, Audience: CallerAudience, IssuedAt: now - 3600, ExpiresAt: now - 1}), "tenant-a", codes.Unauthenticated},
		{"future issued", issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject, Audience: CallerAudience, IssuedAt: now + 60, ExpiresAt: now + 3600}), "tenant-a", codes.Unauthenticated},
		{"missing expiry", issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject, Audience: CallerAudience, OmitExp: true}), "tenant-a", codes.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &pb.GetPoolRequest{Namespace: tc.namespace, PoolName: "pool"}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if tc.token != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tc.token)
			}
			_, err := pb.NewFastPathServiceClient(conn).GetPool(ctx, req)
			require.Equal(t, tc.code, status.Code(err), "native gRPC")
			payload, err := proto.Marshal(req)
			require.NoError(t, err)
			frame := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(payload)))
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, web.URL+pb.FastPathService_GetPool_FullMethodName, bytes.NewReader(append(frame, payload...)))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/grpc-web+proto")
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			response, err := web.Client().Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			if tc.code == codes.OK {
				require.Contains(t, string(body), "grpc-status: 0\r\n")
			} else {
				require.Equal(t, strconv.Itoa(int(tc.code)), response.Header.Get("Grpc-Status"), "gRPC-Web")
			}
		})
	}
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer "+valid, "authorization", "Bearer "+valid))
	_, err = auth.authenticate(ctx)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestCallerClaimsRequireNumericBoundedLifetime(t *testing.T) {
	now := time.Unix(10000, 0)
	for _, tc := range []struct {
		issued, expires any
		valid           bool
	}{
		{float64(10000), float64(13600), true},
		{float64(10000), float64(13601), false},
		{nil, float64(13600), false},
		{"10000", float64(13600), false},
		{float64(10000), "13600", false},
		{float64(10000), float64(10000), false},
		{float64(10031), float64(13600), false},
	} {
		err := validateCallerClaims(jwt.MapClaims{"iat": tc.issued, "exp": tc.expires, "aud": CallerAudience}, now)
		require.Equal(t, tc.valid, err == nil)
	}
}

func TestCallerMethodInventoryMatchesService(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	auth, err := NewCallerAuthenticator(issuer.URL(), []CallerGrant{
		{Subject: serverSubject, Profile: "server", Namespaces: []string{"tenant-a"}},
		{Subject: gatewaySubject, Profile: "gateway", Namespaces: []string{"tenant-a"}},
	}, &http.Client{Timeout: time.Second})
	require.NoError(t, err)
	serverCtx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer "+issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject, Audience: CallerAudience})))
	gatewayCtx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer "+issuer.Mint(fastletauthtest.MintOptions{Subject: gatewaySubject, Audience: CallerAudience})))
	requests := []proto.Message{
		&pb.CreateSandboxRequest{}, &pb.CreateRootfsSandboxRequest{}, &pb.GetSandboxRequest{}, &pb.DeleteRequest{}, &pb.UpdateSandboxRequest{},
		&pb.ListSandboxesRequest{}, &pb.SandboxDiagnosticsRequest{}, &pb.ResolveEndpointRequest{}, &pb.GetPoolRequest{},
		&pb.ListPoolsRequest{}, &pb.CreateSandboxSnapshotRequest{}, &pb.GetSandboxSnapshotRequest{}, &pb.DeleteSandboxSnapshotRequest{},
		&pb.PauseSandboxRequest{}, &pb.ResumeSandboxRequest{}, &pb.InvokeHandlerActionRequest{},
	}
	known := map[string]bool{}
	for _, request := range requests {
		err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(`{"namespace":"tenant-a","create":{"namespace":"tenant-a"},"sandbox":{"namespacedName":{"namespace":"tenant-a"}},"snapshot":{"namespace":"tenant-a","namespacedName":{"namespace":"tenant-a"}}}`), request)
		require.NoError(t, err)
		method, namespace := callerTarget(request)
		require.Equal(t, "tenant-a", namespace, method)
		known[method] = true
		info := &grpc.UnaryServerInfo{FullMethod: "/fastpath.v2.FastPathService/" + method}
		handler := func(ctx context.Context, _ any) (any, error) {
			require.NotEmpty(t, ctx.Value(callerSubjectKey{}))
			return nil, nil
		}
		_, err = auth.UnaryInterceptor(serverCtx, request, info, handler)
		require.NoError(t, err, method)
		_, err = auth.UnaryInterceptor(gatewayCtx, request, info, handler)
		if method == "ResolveEndpoint" || method == "GetSandbox" || method == "ResumeSandbox" {
			require.NoError(t, err, method)
		} else {
			require.Equal(t, codes.PermissionDenied, status.Code(err), method)
		}
	}
	for _, method := range pb.FastPathService_ServiceDesc.Methods {
		require.True(t, known[method.MethodName], method.MethodName)
	}
	method, _ := callerTarget(struct{}{})
	require.Empty(t, method)
	delete(auth.policy, serverSubject)
	_, err = auth.UnaryInterceptor(serverCtx, &pb.GetPoolRequest{Namespace: "tenant-a"}, &grpc.UnaryServerInfo{FullMethod: pb.FastPathService_GetPool_FullMethodName}, func(context.Context, any) (any, error) { t.Fatal("removed subject reached handler"); return nil, nil })
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestCallerRootfsCreateNamespaceScope(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	for _, scope := range []string{"tenant-a", "*"} {
		t.Run(scope, func(t *testing.T) {
			auth, err := NewCallerAuthenticator(issuer.URL(), []CallerGrant{
				{Subject: serverSubject, Profile: "server", Namespaces: []string{scope}},
			}, &http.Client{Timeout: time.Second})
			require.NoError(t, err)
			ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer "+issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject, Audience: CallerAudience})))
			for _, tc := range []struct {
				name   string
				create *pb.CreateSandboxRequest
				code   codes.Code
			}{
				{"missing create", nil, codes.PermissionDenied},
				{"empty namespace", &pb.CreateSandboxRequest{}, codes.PermissionDenied},
				{"allowed namespace", &pb.CreateSandboxRequest{Namespace: "tenant-a"}, codes.OK},
				{"other namespace", &pb.CreateSandboxRequest{Namespace: "tenant-b"}, codes.PermissionDenied},
			} {
				t.Run(tc.name, func(t *testing.T) {
					expected := tc.code
					if scope == "*" && tc.create.GetNamespace() != "" {
						expected = codes.OK
					}
					called := false
					_, err := auth.UnaryInterceptor(ctx, &pb.CreateRootfsSandboxRequest{Create: tc.create}, &grpc.UnaryServerInfo{FullMethod: pb.FastPathService_CreateRootfsSandbox_FullMethodName}, func(context.Context, any) (any, error) {
						called = true
						return nil, nil
					})
					require.Equal(t, expected, status.Code(err))
					require.Equal(t, expected == codes.OK, called)
				})
			}
		})
	}
}

func TestCallerKeyRefreshDiscardsOldIdentityCache(t *testing.T) {
	issuer := fastletauthtest.NewIssuer(t)
	auth, err := NewCallerAuthenticator(issuer.URL(), []CallerGrant{{Subject: serverSubject, Profile: "server", Namespaces: []string{"tenant-a"}}}, &http.Client{Timeout: time.Second})
	require.NoError(t, err)
	token := issuer.Mint(fastletauthtest.MintOptions{Subject: serverSubject, Audience: CallerAudience})
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer "+token))
	_, err = auth.authenticate(ctx)
	require.NoError(t, err)
	issuer.RemoveKey(fastletauthtest.DefaultRSAKeyID)
	auth.loaded = time.Now().Add(-6 * time.Minute)
	old := auth.verifier
	_, err = auth.current()
	require.NoError(t, err)
	require.Eventually(t, func() bool { auth.mu.Lock(); defer auth.mu.Unlock(); return !auth.refreshing && auth.verifier != old }, 2*time.Second, time.Millisecond)
	_, err = auth.authenticate(ctx)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	auth.mu.Lock()
	auth.loaded = time.Now().Add(-16 * time.Minute)
	auth.retry = time.Now().Add(time.Hour)
	auth.mu.Unlock()
	_, err = auth.current()
	require.Equal(t, codes.Unavailable, status.Code(err))
}
