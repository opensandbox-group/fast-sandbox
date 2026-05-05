package env

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

type configCaptureRunner struct {
	fakeRunner
	configContent string
}

func (r *configCaptureRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	for i, arg := range args {
		if arg == "-f" && i+1 < len(args) {
			data, err := os.ReadFile(args[i+1])
			if err != nil {
				return nil, err
			}
			r.configContent = string(data)
		}
	}
	return r.fakeRunner.Run(ctx, dir, name, args...)
}

type sequenceRunner struct {
	commands []recordedCommand
	outputs  [][]byte
}

func (r *sequenceRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, recordedCommand{
		dir:  dir,
		name: name,
		args: append([]string(nil), args...),
	})
	if len(r.outputs) == 0 {
		return []byte(`{"phase":"Running","sandbox_id":"sb-id","fastlet_pod":"fastlet-pod"}`), nil
	}
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, nil
}

func TestFastctlRunWritesConfigAndInvokesCLI(t *testing.T) {
	runner := &configCaptureRunner{}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
		WithFastctlConfigDir(t.TempDir()),
	)

	_, err := client.Run(context.Background(), "sb-cli", FastctlConfig{
		Image:           "docker.io/library/alpine:latest",
		PoolRef:         "pool-a",
		ConsistencyMode: "strong",
		Command:         []string{"/bin/sh"},
		Args:            []string{"-c", "echo FSB_OK && sleep 60"},
		Envs: map[string]string{
			"TEST_VAR": "hello",
		},
		WorkingDir: "/tmp",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	assertCommand(t, runner.commands, "/repo/bin/fastctl",
		"--endpoint", "127.0.0.1:19090",
		"--namespace", "tenant-a",
		"run", "sb-cli", "-f", runner.commands[0].args[len(runner.commands[0].args)-1],
	)

	for _, want := range []string{
		"image: docker.io/library/alpine:latest",
		"pool_ref: pool-a",
		"consistency_mode: strong",
		"command:",
		"- /bin/sh",
		"args:",
		"- -c",
		"echo FSB_OK && sleep 60",
		"TEST_VAR: hello",
		"working_dir: /tmp",
	} {
		if !strings.Contains(runner.configContent, want) {
			t.Fatalf("config missing %q:\n%s", want, runner.configContent)
		}
	}
}

func TestFastctlGetLogsAndDeleteInvokeCLI(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "get", "sb-cli", "-o", "json"): `{"phase":"Running"}`,
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "logs", "sb-cli"):              "FSB_OK\n",
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "delete", "sb-cli"):            "deleted\n",
		},
		errs: map[string]error{},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
	)

	if _, err := client.GetJSON(context.Background(), "sb-cli"); err != nil {
		t.Fatalf("GetJSON returned error: %v", err)
	}
	logs, err := client.Logs(context.Background(), "sb-cli")
	if err != nil {
		t.Fatalf("Logs returned error: %v", err)
	}
	if logs != "FSB_OK\n" {
		t.Fatalf("Logs = %q, want FSB_OK", logs)
	}
	if err := client.Delete(context.Background(), "sb-cli"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "get", "sb-cli", "-o", "json")
	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "logs", "sb-cli")
	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "delete", "sb-cli")
}

func TestFastctlGetJSONIgnoresCLIConfigPreamble(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "get", "sb-cli", "-o", "json"): "Using config file: /repo/.fastctl/config.json\n{\"sandbox_name\":\"sb-cli\",\"phase\":\"Running\"}\n",
		},
		errs: map[string]error{},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
	)

	info, err := client.GetJSON(context.Background(), "sb-cli")
	if err != nil {
		t.Fatalf("GetJSON returned error: %v", err)
	}
	if info.SandboxName != "sb-cli" || info.Phase != "Running" {
		t.Fatalf("info = %+v, want sandbox_name sb-cli phase Running", info)
	}
}

func TestFastctlWaitRunningRequiresSandboxIDAndFastletPod(t *testing.T) {
	runner := &sequenceRunner{
		outputs: [][]byte{
			[]byte(`{"phase":"Running"}`),
			[]byte(`{"phase":"Running","sandbox_id":"sb-id","fastlet_pod":"fastlet-pod"}`),
		},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := client.WaitRunning(ctx, "sb-cli")
	if err != nil {
		t.Fatalf("WaitRunning returned error: %v", err)
	}
	if info.SandboxID != "sb-id" || info.FastletPod != "fastlet-pod" {
		t.Fatalf("info = %+v, want sandbox ID and fastlet pod", info)
	}
	if len(runner.commands) < 2 {
		t.Fatalf("expected WaitRunning to keep polling until sandbox ID and fastlet pod are set")
	}
}

func TestFastctlUpdateLabelsAndResetInvokeCLI(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "update", "sb-cli", "--labels", "test=e2e,env=cli"): "updated\n",
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "reset", "sb-cli"):                                  "reset\n",
		},
		errs: map[string]error{},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
	)

	if _, err := client.UpdateLabels(context.Background(), "sb-cli", "test=e2e", "env=cli"); err != nil {
		t.Fatalf("UpdateLabels returned error: %v", err)
	}
	if _, err := client.Reset(context.Background(), "sb-cli"); err != nil {
		t.Fatalf("Reset returned error: %v", err)
	}

	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "update", "sb-cli", "--labels", "test=e2e,env=cli")
	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "reset", "sb-cli")
}
