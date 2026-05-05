package env

import "testing"

func TestProfileSettings(t *testing.T) {
	tests := []struct {
		name        string
		profile     Profile
		clusterName string
		kindConfig  string
		kindImage   string
		runtime     RuntimeKind
	}{
		{
			name:        "basic",
			profile:     ProfileBasic,
			clusterName: "fsb-e2e-basic",
			kindConfig:  "",
			kindImage:   "kindest/node:v1.27.3",
			runtime:     RuntimeContainer,
		},
		{
			name:        "gvisor",
			profile:     ProfileGVisor,
			clusterName: "fsb-e2e-gvisor",
			kindConfig:  "test/e2e/manifests/kind/gvisor.yaml",
			kindImage:   "kindest/node:v1.31.0",
			runtime:     RuntimeGVisor,
		},
		{
			name:        "kata qemu",
			profile:     ProfileKataQemu,
			clusterName: "fsb-e2e-kata",
			kindConfig:  "test/e2e/manifests/kind/kata.yaml",
			kindImage:   "kindest/node:v1.31.0",
			runtime:     RuntimeKataQemu,
		},
		{
			name:        "kata clh",
			profile:     ProfileKataClh,
			clusterName: "fsb-e2e-kata",
			kindConfig:  "test/e2e/manifests/kind/kata.yaml",
			kindImage:   "kindest/node:v1.31.0",
			runtime:     RuntimeKataClh,
		},
		{
			name:        "kata fc",
			profile:     ProfileKataFc,
			clusterName: "fsb-e2e-kata",
			kindConfig:  "test/e2e/manifests/kind/kata.yaml",
			kindImage:   "kindest/node:v1.31.0",
			runtime:     RuntimeKataFc,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings, err := tt.profile.Settings()
			if err != nil {
				t.Fatalf("Settings returned error: %v", err)
			}
			if settings.ClusterName != tt.clusterName {
				t.Fatalf("ClusterName = %q, want %q", settings.ClusterName, tt.clusterName)
			}
			if settings.KindConfig != tt.kindConfig {
				t.Fatalf("KindConfig = %q, want %q", settings.KindConfig, tt.kindConfig)
			}
			if settings.KindImage != tt.kindImage {
				t.Fatalf("KindImage = %q, want %q", settings.KindImage, tt.kindImage)
			}
			if settings.Runtime != tt.runtime {
				t.Fatalf("Runtime = %q, want %q", settings.Runtime, tt.runtime)
			}
		})
	}
}

func TestUnknownProfileSettingsReturnsError(t *testing.T) {
	_, err := Profile("mystery").Settings()
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
}
