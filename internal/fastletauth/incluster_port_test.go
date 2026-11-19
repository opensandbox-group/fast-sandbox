package fastletauth

import "testing"

func TestApplyInClusterAPIServerPort(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "6443")
	cases := []struct{ in, want string }{
		{"https://kubernetes.default.svc/.well-known/openid-configuration",
			"https://kubernetes.default.svc:6443/.well-known/openid-configuration"},
		{"https://kubernetes.default.svc:443/x", "https://kubernetes.default.svc:443/x"},
		{"https://other.example.com/x", "https://other.example.com/x"},
	}
	for _, tc := range cases {
		if got := applyInClusterAPIServerPort(tc.in); got != tc.want {
			t.Errorf("applyInClusterAPIServerPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if got := applyInClusterAPIServerPort("https://kubernetes.default.svc/x"); got != "https://kubernetes.default.svc/x" {
		t.Errorf("unset port must be a no-op, got %q", got)
	}
}
