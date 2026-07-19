package fastpathv1

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestCreateRequestWireNumbersRemainStable(t *testing.T) {
	descriptor := (&CreateRequest{}).ProtoReflect().Descriptor().Fields()
	want := map[protoreflect.FieldNumber]protoreflect.Name{
		1: "image", 2: "pool_ref", 3: "exposed_ports", 4: "command", 5: "args",
		6: "namespace", 7: "consistency_mode", 8: "name", 9: "envs",
		10: "working_dir", 11: "request_id",
	}
	for number, name := range want {
		field := descriptor.ByNumber(number)
		require.NotNilf(t, field, "field number %d must remain reserved by a field", number)
		require.Equal(t, name, field.Name(), "field number %d was reused", number)
	}
	require.True(t, descriptor.ByNumber(3).Options().(*descriptorpb.FieldOptions).GetDeprecated())
	require.True(t, descriptor.ByNumber(7).Options().(*descriptorpb.FieldOptions).GetDeprecated())
}

func TestFastPathServiceExposesRouteResolution(t *testing.T) {
	methods := File_api_proto_v1_fastpath_proto.Services().ByName("FastPathService").Methods()
	require.NotNil(t, methods.ByName("ResolveEndpoint"))
	require.NotNil(t, methods.ByName("IssueRouteCredential"))
	for _, forbidden := range []protoreflect.Name{
		"Exec", "ExecStream", "FileStat", "FileList", "FileRead", "FileWrite", "FileMkdir", "FileDelete", "PTY",
	} {
		require.Nilf(t, methods.ByName(forbidden), "FastPath must not expose the injected component method %s", forbidden)
	}
}
