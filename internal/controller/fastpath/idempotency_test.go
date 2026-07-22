package fastpath

import (
	"testing"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/stretchr/testify/require"
)

func TestValidateRequestID(t *testing.T) {
	require.NoError(t, ValidateRequestID("018fa5f0-7e5d-7db1-a936-3bb96f98e531"))
	require.Error(t, ValidateRequestID(""))
	require.Error(t, ValidateRequestID("contains space"))
	require.Error(t, ValidateRequestID(string(make([]byte, maxRequestIDLength+1))))
}

func TestRequestIDLabelValueIsStableAndLabelSafe(t *testing.T) {
	value := requestIDLabelValue("request-a")
	require.Equal(t, value, requestIDLabelValue("request-a"))
	require.NotEqual(t, value, requestIDLabelValue("request-b"))
	require.Len(t, value, 32)
}

func TestCreateSpecHashIsDeterministic(t *testing.T) {
	a := &fastpathv1.CreateRequest{
		RequestId: "request-a",
		Image:     "example/image:v1",
		PoolRef:   "default",
		Envs:      map[string]string{"B": "2", "A": "1"},
	}
	b := &fastpathv1.CreateRequest{
		RequestId: "request-b",
		Image:     "example/image:v1",
		PoolRef:   "default",
		Namespace: "default",
		Envs:      map[string]string{"A": "1", "B": "2"},
	}

	hashA, err := CreateSpecHash(a)
	require.NoError(t, err)
	hashB, err := CreateSpecHash(b)
	require.NoError(t, err)
	require.Equal(t, hashA, hashB)

	b.Image = "example/image:v2"
	hashChanged, err := CreateSpecHash(b)
	require.NoError(t, err)
	require.NotEqual(t, hashA, hashChanged)
}
