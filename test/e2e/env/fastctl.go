package env

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type FastctlConfig struct {
	Image      string            `yaml:"image"`
	PoolRef    string            `yaml:"pool_ref"`
	Command    []string          `yaml:"command,omitempty"`
	Args       []string          `yaml:"args,omitempty"`
	Envs       map[string]string `yaml:"envs,omitempty"`
	WorkingDir string            `yaml:"working_dir,omitempty"`
}

type SandboxInfo struct {
	SandboxUID       string `json:"sandbox_uid"`
	SandboxName      string `json:"sandbox_name"`
	RuntimeState     string `json:"runtime_state"`
	DataPlaneState   string `json:"data_plane_state"`
	UserProcessState string `json:"user_process_state"`
	FastletPod       string `json:"fastlet_pod"`
	Image            string `json:"image"`
	PoolRef          string `json:"pool_ref"`
	CreatedAt        int64  `json:"created_at"`
}

type FastctlOption func(*Fastctl)

type Fastctl struct {
	runner     Runner
	rootDir    string
	binaryPath string
	endpoint   string
	proxyURL   string
	namespace  string
	configDir  string
}

func NewFastctl(opts ...FastctlOption) *Fastctl {
	rootDir, _ := findRootDir()
	client := &Fastctl{
		runner:     execRunner{},
		rootDir:    rootDir,
		binaryPath: filepath.Join(rootDir, "bin", "fastctl"),
		endpoint:   "localhost:9090",
		namespace:  "default",
		configDir:  os.TempDir(),
	}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

func WithFastctlRunner(runner Runner) FastctlOption {
	return func(client *Fastctl) {
		if runner != nil {
			client.runner = runner
		}
	}
}

func WithFastctlBinary(binaryPath string) FastctlOption {
	return func(client *Fastctl) {
		if binaryPath != "" {
			client.binaryPath = binaryPath
		}
	}
}

func WithFastctlRootDir(rootDir string) FastctlOption {
	return func(client *Fastctl) {
		if rootDir != "" {
			client.rootDir = rootDir
		}
	}
}

func WithFastctlEndpoint(endpoint string) FastctlOption {
	return func(client *Fastctl) {
		if endpoint != "" {
			client.endpoint = endpoint
		}
	}
}

func WithFastctlProxyEndpoint(proxyURL string) FastctlOption {
	return func(client *Fastctl) {
		if proxyURL != "" {
			client.proxyURL = proxyURL
		}
	}
}

func WithFastctlNamespace(namespace string) FastctlOption {
	return func(client *Fastctl) {
		if namespace != "" {
			client.namespace = namespace
		}
	}
}

func WithFastctlConfigDir(configDir string) FastctlOption {
	return func(client *Fastctl) {
		if configDir != "" {
			client.configDir = configDir
		}
	}
}

func (c *Fastctl) Run(ctx context.Context, name string, config FastctlConfig) ([]byte, error) {
	if err := os.MkdirAll(c.configDir, 0755); err != nil {
		return nil, fmt.Errorf("create fastctl config dir: %w", err)
	}
	file, err := os.CreateTemp(c.configDir, "fastctl-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("create fastctl config: %w", err)
	}
	configPath := file.Name()
	defer os.Remove(configPath)

	data, err := yaml.Marshal(config)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("marshal fastctl config: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return nil, fmt.Errorf("write fastctl config: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close fastctl config: %w", err)
	}

	return c.run(ctx, "run", name, "-f", configPath)
}

func (c *Fastctl) GetJSON(ctx context.Context, name string) (*SandboxInfo, error) {
	output, err := c.run(ctx, "get", name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var info SandboxInfo
	payload := jsonPayload(output)
	if err := json.Unmarshal(payload, &info); err != nil {
		return nil, fmt.Errorf("parse fastctl get output: %w\n%s", err, string(output))
	}
	return &info, nil
}

func (c *Fastctl) UpdateLabels(ctx context.Context, name string, labels ...string) ([]byte, error) {
	return c.run(ctx, "update", name, "--labels", strings.Join(labels, ","))
}

func (c *Fastctl) Reset(ctx context.Context, name string) ([]byte, error) {
	return c.run(ctx, "reset", name)
}

func (c *Fastctl) Delete(ctx context.Context, name string) error {
	_, err := c.run(ctx, "delete", name)
	return err
}

// Command runs a raw fastctl subcommand with this client's configured
// Fast-Path, Sandbox Proxy, and namespace endpoints.
func (c *Fastctl) Command(ctx context.Context, args ...string) ([]byte, error) {
	return c.run(ctx, args...)
}

func (c *Fastctl) WaitRunning(ctx context.Context, name string) (*SandboxInfo, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		info, err := c.GetJSON(ctx, name)
		if err == nil && info.RuntimeState == "Ready" && info.DataPlaneState == "Ready" && info.SandboxUID != "" && info.FastletPod != "" {
			return info, nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("wait for sandbox %s running: %w; last get error: %v", name, ctx.Err(), err)
			}
			return nil, fmt.Errorf("wait for sandbox %s running: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Fastctl) run(ctx context.Context, args ...string) ([]byte, error) {
	commandArgs := []string{
		"--endpoint", c.endpoint,
		"--namespace", c.namespace,
	}
	if c.proxyURL != "" {
		commandArgs = append(commandArgs, "--proxy-endpoint", c.proxyURL)
	}
	commandArgs = append(commandArgs, args...)
	output, err := c.runner.Run(ctx, c.rootDir, c.binaryPath, commandArgs...)
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w\n%s", commandString(c.binaryPath, commandArgs...), err, string(output))
	}
	return output, nil
}

func jsonPayload(output []byte) []byte {
	index := strings.IndexByte(string(output), '{')
	if index == -1 {
		return output
	}
	return output[index:]
}
