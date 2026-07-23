package infracatalog

import (
	"crypto/sha256"
	"fmt"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/runtimecatalog"
)

const (
	builtinVersion = "v1"
	// TestInfraScript is an e2e-only component artifact. It intentionally
	// relies on BusyBox nc and is never selected by production profiles. The
	// service requires the per-instance credential, proving that Fastlet Proxy
	// injects upstream authentication without exposing it to the caller.
	TestInfraScript = `#!/bin/sh
set -eu
root=/tmp/fast-sandbox-test-infra
mkdir -p "$root"
cat > "$root/serve" <<'EOF'
#!/bin/sh
set -eu
request_path=/
request_method=
request_token=
while IFS= read -r line; do
  line="$(printf '%s' "$line" | tr -d '\r')"
  [ -n "$line" ] || break
  case "$line" in
    GET\ *|POST\ *|DELETE\ *)
      request_method="${line%% *}"
      request_path="${line#* }"
      request_path="${request_path%% *}"
      ;;
    X-Fast-Sandbox-Infra-Token:\ *)
      request_token="${line#X-Fast-Sandbox-Infra-Token: }"
      ;;
  esac
done
content_type=text/plain
if [ "$request_token" != "$FAST_SANDBOX_INTERNAL_TOKEN" ]; then
  status='401 Unauthorized'
  body=unauthorized
elif [ "$request_path" = /health ]; then
  status='200 OK'
  body=ready
elif [ "$request_path" = /value ]; then
  status='200 OK'
  body=test-infra
elif [ "$request_method" = POST ] && [ "$request_path" = /command ]; then
  status='200 OK'
  content_type=text/event-stream
  body='data: {"type":"init","text":"cmd-e2e"}

data: {"type":"stdout","text":"sdk-exec\n"}

data: {"type":"execution_complete","exit_code":0}

'
elif [ "$request_method" = GET ] && [ "${request_path%%\?*}" = /files/info ]; then
  status='200 OK'
  content_type=application/json
  body='{"/tmp/value":{"path":"/tmp/value","size":8,"mode":644}}'
elif [ "$request_method" = GET ] && [ "${request_path%%\?*}" = /files/download ]; then
  status='200 OK'
  body=sdk-file
elif [ "$request_method" = GET ] && [ "${request_path%%\?*}" = /directories/list ]; then
  status='200 OK'
  content_type=application/json
  body='[{"path":"/tmp/value","size":8,"mode":644}]'
elif { [ "$request_method" = POST ] || [ "$request_method" = DELETE ]; }; then
  status='200 OK'
  body=
else
  status='404 Not Found'
  body=not-found
fi
printf 'HTTP/1.1 %s\r\nContent-Type: %s\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' "$status" "$content_type" "${#body}" "$body"
EOF
chmod 0700 "$root/serve"
exec nc -lk -p 18080 -e "$root/serve"
`
)

func TestInfraDigest() string {
	digest := sha256.Sum256([]byte(TestInfraScript))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func builtinProfiles() map[string]Profile {
	return map[string]Profile{
		"minimal": {
			Name: "minimal", Version: builtinVersion, Configured: true,
		},
		"test-infra": {
			Name: "test-infra", Version: builtinVersion, Configured: true,
			AllowedRuntimes: []apiv1alpha1.RuntimeName{apiv1alpha1.RuntimeContainer},
			Components: []Component{{
				Name:          "test-infra",
				Artifact:      Artifact{SourceType: SourceEmbedded, Reference: "embedded://test-infra/v1", Digest: TestInfraDigest(), Executable: true},
				ContainerPath: "/.fast/infra/test-infra",
				DeliveryModes: []runtimecatalog.InfraDeliveryMode{runtimecatalog.InfraDeliveryBindMount},
				Activation:    Activation{Mode: ActivationEntrypointSupervisor, Command: "/.fast/infra/test-infra", RestartPolicy: RestartOnFailure},
				InstanceInit:  InstanceInit{Mode: InitNone},
				Services: []Service{{
					Name: "test-infra", Transport: "http", Port: 18080,
					Readiness: ReadinessProbe{Type: ProbeHTTP, Path: "/health", Timeout: 5 * time.Second, Interval: 50 * time.Millisecond},
				}},
				Required: true,
			}},
		},
		"opensandbox-execd": {
			Name: "opensandbox-execd", Version: builtinVersion, Configured: false,
			UnavailableReason: "an immutable execd OCI reference and digest must be supplied by the platform release",
			AllowedRuntimes:   []apiv1alpha1.RuntimeName{apiv1alpha1.RuntimeContainer, apiv1alpha1.RuntimeGVisor},
			Components: []Component{{
				Name: "execd",
				Artifact: Artifact{
					SourceType: SourceOCIArtifact, Reference: "oci://platform/opensandbox-execd",
					Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000", Executable: true,
				},
				ContainerPath: "/.fast/infra/opensandbox",
				DeliveryModes: []runtimecatalog.InfraDeliveryMode{runtimecatalog.InfraDeliveryBindMount},
				Activation:    Activation{Mode: ActivationComponentBootstrap, Command: "/.fast/infra/opensandbox/bootstrap.sh", RestartPolicy: RestartOnFailure},
				InstanceInit:  InstanceInit{Mode: InitEnvironment},
				Services: []Service{{
					Name: "execd", Transport: "http", Port: 44772,
					Readiness: ReadinessProbe{Type: ProbeHTTP, Path: "/ping", Timeout: 10 * time.Second, Interval: 100 * time.Millisecond},
				}},
				Required: true,
			}},
		},
	}
}
