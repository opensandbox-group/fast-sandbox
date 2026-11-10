package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Exercise the actual workflow script without credentials or network access.
// In particular, a maintainer triggering an external PR must not authorize it:
// the API lookup must use the PR author, not the triggering actor.
func TestIntegrationAuthorPermission(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/opensandbox-integration.yml")
	require.NoError(t, err)
	var workflow struct {
		Jobs map[string]struct {
			Needs string `yaml:"needs"`
			If    string `yaml:"if"`
			Steps []struct {
				ID  string            `yaml:"id"`
				Env map[string]string `yaml:"env"`
				Run string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &workflow))
	authorize := workflow.Jobs["authorize"]
	require.Len(t, authorize.Steps, 1)
	step := authorize.Steps[0]
	require.Equal(t, "permission", step.ID)
	require.Equal(t, "${{ github.event.pull_request.user.login }}", step.Env["PR_AUTHOR"])
	require.Equal(t, "authorize", workflow.Jobs["integration"].Needs)
	require.Equal(t, "needs.authorize.outputs.allowed == 'true'", workflow.Jobs["integration"].If)
	require.NotContains(t, string(raw), "author_association")

	for _, test := range []struct {
		name       string
		event      string
		approved   bool
		status     string
		body       string
		curlExit   string
		wantOutput string
		wantError  bool
	}{
		{name: "private organization admin", body: `{"permission":"admin"}`, wantOutput: "allowed=true"},
		{name: "maintainer", body: `{"permission":"maintain"}`, wantOutput: "allowed=true"},
		{name: "writer", body: `{"permission":"write"}`, wantOutput: "allowed=true"},
		{name: "reader", body: `{"permission":"read"}`, wantOutput: "allowed=false"},
		{name: "triage", body: `{"permission":"triage"}`, wantOutput: "allowed=false"},
		{name: "no access", body: `{"permission":"none"}`, wantOutput: "allowed=false"},
		{name: "unknown role", body: `{"permission":"custom-unknown"}`, wantOutput: "allowed=false"},
		{name: "non collaborator", status: "404", body: `{}`, wantOutput: "allowed=false"},
		{name: "label approval", approved: true, wantOutput: "allowed=true"},
		{name: "manual dispatch", event: "workflow_dispatch", wantOutput: "allowed=true"},
		{name: "unauthorized API", status: "401", wantError: true},
		{name: "forbidden API", status: "403", wantError: true},
		{name: "rate limited API", status: "429", wantError: true},
		{name: "unavailable API", status: "503", wantError: true},
		{name: "transport failure", curlExit: "28", wantError: true},
		{name: "malformed response", body: `not json`, wantError: true},
		{name: "missing permission", body: `{}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			mock := `#!/bin/bash
set -euo pipefail
printf '%s\n' "$@" > "$MOCK_ARGS"
while [[ $# -gt 0 ]]; do
  if [[ "$1" == '-o' ]]; then
    printf '%s' "$MOCK_BODY" > "$2"
    shift
  fi
  shift
done
printf '%s' "$MOCK_STATUS"
exit "$MOCK_EXIT"
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "curl"), []byte(mock), 0700))
			event := test.event
			if event == "" {
				event = "pull_request_target"
			}
			status := test.status
			if status == "" {
				status = "200"
			}
			curlExit := test.curlExit
			if curlExit == "" {
				curlExit = "0"
			}
			approved := "false"
			if test.approved {
				approved = "true"
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			for name, value := range map[string]string{
				"EVENT_NAME": event, "APPROVED": approved, "PR_AUTHOR": "pr-author",
				"GITHUB_ACTOR": "different-admin", "GH_TOKEN": "test-token",
				"GITHUB_API_URL": "https://api.github.invalid", "GITHUB_REPOSITORY": "owner/repo",
				"RUNNER_TEMP": dir, "GITHUB_OUTPUT": filepath.Join(dir, "output"),
				"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
				"MOCK_ARGS":           filepath.Join(dir, "args"), "MOCK_BODY": test.body,
				"MOCK_STATUS": status, "MOCK_EXIT": curlExit,
			} {
				t.Setenv(name, value)
			}
			log, err := exec.Command("bash", "-c", step.Run).CombinedOutput()
			if test.wantError {
				require.Error(t, err, string(log))
				require.NoFileExists(t, filepath.Join(dir, "output"), "API errors must not emit an authorization decision")
			} else {
				require.NoError(t, err, string(log))
				output, err := os.ReadFile(filepath.Join(dir, "output"))
				require.NoError(t, err)
				require.Equal(t, test.wantOutput, strings.TrimSpace(string(output)))
			}
			if test.approved || event == "workflow_dispatch" {
				require.NoFileExists(t, filepath.Join(dir, "args"), "explicit approval does not require the permission API")
			} else {
				args, err := os.ReadFile(filepath.Join(dir, "args"))
				require.NoError(t, err)
				require.Contains(t, string(args), "https://api.github.invalid/repos/owner/repo/collaborators/pr-author/permission\n")
				require.NotContains(t, string(args), "different-admin")
			}
		})
	}
}
