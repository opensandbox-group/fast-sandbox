package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFastletProxyResources(t *testing.T) {
	tests := []struct {
		name          string
		requestCPU    string
		requestMemory string
		limitCPU      string
		limitMemory   string
		want          [4]string
		wantError     string
	}{
		{
			name: "custom resources", requestCPU: "250m", requestMemory: "1Gi", limitCPU: "1", limitMemory: "4Gi",
			want: [4]string{"250m", "1Gi", "1", "4Gi"},
		},
		{
			name: "invalid quantity", requestCPU: "bad", requestMemory: "64Mi", limitCPU: "250m", limitMemory: "256Mi",
			wantError: "--fastlet-proxy-request-cpu must be a Kubernetes resource quantity",
		},
		{
			name: "zero quantity", requestCPU: "0", requestMemory: "64Mi", limitCPU: "250m", limitMemory: "256Mi",
			wantError: "--fastlet-proxy-request-cpu must be greater than zero",
		},
		{
			name: "request exceeds limit", requestCPU: "2", requestMemory: "1Gi", limitCPU: "1", limitMemory: "4Gi",
			wantError: "fastlet proxy request cpu=2 exceeds limit 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resources, err := parseFastletProxyResources(tt.requestCPU, tt.requestMemory, tt.limitCPU, tt.limitMemory)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want[0], resources.Requests.Cpu().String())
			require.Equal(t, tt.want[1], resources.Requests.Memory().String())
			require.Equal(t, tt.want[2], resources.Limits.Cpu().String())
			require.Equal(t, tt.want[3], resources.Limits.Memory().String())
		})
	}
}
