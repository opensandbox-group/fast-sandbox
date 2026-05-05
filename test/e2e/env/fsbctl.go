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

type FSBCtlConfig struct {
	Image           string            `yaml:"image"`
	PoolRef         string            `yaml:"pool_ref"`
	ConsistencyMode string            `yaml:"consistency_mode"`
	Command         []string          `yaml:"command,omitempty"`
	Args            []string          `yaml:"args,omitempty"`
	ExposedPorts    []int32           `yaml:"exposed_ports,omitempty"`
	Envs            map[string]string `yaml:"envs,omitempty"`
	WorkingDir      string            `yaml:"working_dir,omitempty"`
}

type SandboxInfo struct {
	SandboxID   string   `json:"sandbox_id"`
	SandboxName string   `json:"sandbox_name"`
	Phase       string   `json:"phase"`
	AgentPod    string   `json:"agent_pod"`
	Endpoints   []string `json:"endpoints"`
	Image       string   `json:"image"`
	PoolRef     string   `json:"pool_ref"`
	CreatedAt   int64    `json:"created_at"`
}

type FSBCtlOption func(*FSBCtl)

type FSBCtl struct {
	runner     Runner
	rootDir    string
	binaryPath string
	endpoint   string
	namespace  string
	configDir  string
}

func NewFSBCtl(opts ...FSBCtlOption) *FSBCtl {
	rootDir, _ := findRootDir()
	client := &FSBCtl{
		runner:     execRunner{},
		rootDir:    rootDir,
		binaryPath: filepath.Join(rootDir, "bin", "fsb-ctl"),
		endpoint:   "localhost:9090",
		namespace:  "default",
		configDir:  os.TempDir(),
	}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

func WithFSBCtlRunner(runner Runner) FSBCtlOption {
	return func(client *FSBCtl) {
		if runner != nil {
			client.runner = runner
		}
	}
}

func WithFSBCtlBinary(binaryPath string) FSBCtlOption {
	return func(client *FSBCtl) {
		if binaryPath != "" {
			client.binaryPath = binaryPath
		}
	}
}

func WithFSBCtlRootDir(rootDir string) FSBCtlOption {
	return func(client *FSBCtl) {
		if rootDir != "" {
			client.rootDir = rootDir
		}
	}
}

func WithFSBCtlEndpoint(endpoint string) FSBCtlOption {
	return func(client *FSBCtl) {
		if endpoint != "" {
			client.endpoint = endpoint
		}
	}
}

func WithFSBCtlNamespace(namespace string) FSBCtlOption {
	return func(client *FSBCtl) {
		if namespace != "" {
			client.namespace = namespace
		}
	}
}

func WithFSBCtlConfigDir(configDir string) FSBCtlOption {
	return func(client *FSBCtl) {
		if configDir != "" {
			client.configDir = configDir
		}
	}
}

func (c *FSBCtl) Run(ctx context.Context, name string, config FSBCtlConfig) ([]byte, error) {
	if config.ConsistencyMode == "" {
		config.ConsistencyMode = "strong"
	}
	if err := os.MkdirAll(c.configDir, 0755); err != nil {
		return nil, fmt.Errorf("create fsb-ctl config dir: %w", err)
	}
	file, err := os.CreateTemp(c.configDir, "fsb-ctl-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("create fsb-ctl config: %w", err)
	}
	configPath := file.Name()
	defer os.Remove(configPath)

	data, err := yaml.Marshal(config)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("marshal fsb-ctl config: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return nil, fmt.Errorf("write fsb-ctl config: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close fsb-ctl config: %w", err)
	}

	return c.run(ctx, "run", name, "-f", configPath)
}

func (c *FSBCtl) GetJSON(ctx context.Context, name string) (*SandboxInfo, error) {
	output, err := c.run(ctx, "get", name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var info SandboxInfo
	payload := jsonPayload(output)
	if err := json.Unmarshal(payload, &info); err != nil {
		return nil, fmt.Errorf("parse fsb-ctl get output: %w\n%s", err, string(output))
	}
	return &info, nil
}

func (c *FSBCtl) Logs(ctx context.Context, name string) (string, error) {
	output, err := c.run(ctx, "logs", name)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func (c *FSBCtl) UpdateLabels(ctx context.Context, name string, labels ...string) ([]byte, error) {
	return c.run(ctx, "update", name, "--labels", strings.Join(labels, ","))
}

func (c *FSBCtl) Reset(ctx context.Context, name string) ([]byte, error) {
	return c.run(ctx, "reset", name)
}

func (c *FSBCtl) Delete(ctx context.Context, name string) error {
	_, err := c.run(ctx, "delete", name)
	return err
}

func (c *FSBCtl) WaitRunning(ctx context.Context, name string) (*SandboxInfo, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		info, err := c.GetJSON(ctx, name)
		if err == nil && (info.Phase == "Running" || info.Phase == "Bound") && info.SandboxID != "" && info.AgentPod != "" {
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

func (c *FSBCtl) run(ctx context.Context, args ...string) ([]byte, error) {
	commandArgs := []string{
		"--endpoint", c.endpoint,
		"--namespace", c.namespace,
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
