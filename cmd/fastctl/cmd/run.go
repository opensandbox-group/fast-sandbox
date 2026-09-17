package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

type SandboxConfig struct {
	Image                  string                `yaml:"image"`
	PoolRef                string                `yaml:"pool_ref"`
	Command                []string              `yaml:"command,omitempty"`
	Args                   []string              `yaml:"args,omitempty"`
	Envs                   map[string]string     `yaml:"envs,omitempty"`
	WorkingDir             string                `yaml:"working_dir,omitempty"`
	ExpiresAt              int64                 `yaml:"expires_at,omitempty"`
	Metadata               map[string]string     `yaml:"metadata,omitempty"`
	FailurePolicy          string                `yaml:"failure_policy,omitempty"`
	RecoveryTimeoutSeconds int32                 `yaml:"recovery_timeout_seconds,omitempty"`
	ActionBindings         []ActionBindingConfig `yaml:"action_bindings,omitempty"`
}

type ActionBindingConfig struct {
	Handler string `yaml:"handler"`
	Input   string `yaml:"input"`
}

var (
	configFile         string
	pool               string
	image              string
	requestID          string
	runExpiresAt       int64
	runMetadata        []string
	runFailurePolicy   string
	runRecoveryTimeout int32
	runActionBindings  []string
)

var runCmd = &cobra.Command{
	Use:   "run <sandbox-name> [command] [args...]",
	Short: "Create a new sandbox via Fast-Path API",
	Long: `Create a new sandbox using interactive mode, config file, or flags.

Modes:
  1. Interactive: fastctl run my-sandbox (opens editor, caches last edit)
  2. File-based:  fastctl run my-sandbox -f config.yaml
  3. Flag-based:  fastctl run my-sandbox --image=alpine --pool=default-pool

Interactive Cache:
  - First run: shows default template
  - Subsequent runs: loads your last edit
  - Clear cache: rm ~/.fastctl/cache/<sandbox-name>.yaml

Priority: Flags > Config File > Interactive Input
`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		klog.V(4).InfoS("CLI run command started", "name", name)

		config := SandboxConfig{
			PoolRef: "default-pool",
		}

		if configFile != "" {
			klog.V(4).InfoS("loading config from file", "file", configFile)
			data, err := os.ReadFile(configFile)
			if err != nil {
				klog.ErrorS(err, "Failed to read config file", "file", configFile)
				exitWithErrorf("failed to read config file: %v", err)
			}
			if err := yaml.Unmarshal(data, &config); err != nil {
				klog.ErrorS(err, "Failed to parse config file", "file", configFile)
				exitWithErrorf("failed to parse config file: %v", err)
			}
		} else if image == "" {
			fmt.Println("Entering interactive mode...")
			if err := runInteractive(name, &config); err != nil {
				klog.ErrorS(err, "Interactive mode failed", "name", name)
				exitWithErrorf("interactive mode failed: %v", err)
			}
		}

		if image != "" {
			config.Image = image
		}
		if pool != "" && cmd.Flags().Changed("pool") {
			config.PoolRef = pool
		}
		if len(args) > 1 {
			config.Command = args[1:]
		}
		if config.Image == "" {
			klog.ErrorS(nil, "Image is required but not provided", "name", name)
			exitWithErrorf("image is required (via flag, file, or interactive mode)")
		}

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		start := time.Now()
		createRequestID := requestID
		if createRequestID == "" {
			createRequestID = name
		}
		if createRequestID != name {
			exitWithErrorf("--request-id must equal the Sandbox name")
		}
		metadata := make(map[string]string, len(config.Metadata)+len(runMetadata))
		for key, value := range config.Metadata {
			metadata[key] = value
		}
		for _, item := range runMetadata {
			parts := strings.SplitN(item, "=", 2)
			if len(parts) != 2 {
				exitWithErrorf("invalid metadata %q; expected key=value", item)
			}
			metadata[parts[0]] = parts[1]
		}
		failurePolicy, err := parseFailurePolicy(config.FailurePolicy)
		if config.FailurePolicy == "" {
			failurePolicy = fastpathv2.FailurePolicy_MANUAL
		} else if err != nil {
			exitWithError(err)
		}
		if runFailurePolicy != "" {
			failurePolicy, err = parseFailurePolicy(runFailurePolicy)
			if err != nil {
				exitWithError(err)
			}
		}
		expiresAt := config.ExpiresAt
		if cmd.Flags().Changed("expires-at") {
			expiresAt = runExpiresAt
		}
		recoveryTimeout := config.RecoveryTimeoutSeconds
		if cmd.Flags().Changed("recovery-timeout") {
			recoveryTimeout = runRecoveryTimeout
		}
		actionBindings := config.ActionBindings
		if cmd.Flags().Changed("action") {
			actionBindings, err = parseActionBindings(runActionBindings)
			if err != nil {
				exitWithError(err)
			}
		}
		apiBindings := make([]*fastpathv2.ActionBinding, 0, len(actionBindings))
		for _, binding := range actionBindings {
			apiBindings = append(apiBindings, &fastpathv2.ActionBinding{Handler: binding.Handler, Input: binding.Input})
		}
		req := &fastpathv2.CreateSandboxRequest{
			Image:                config.Image,
			PoolRef:              config.PoolRef,
			Namespace:            viper.GetString("namespace"),
			Command:              config.Command,
			Args:                 config.Args,
			Envs:                 config.Envs,
			WorkingDir:           config.WorkingDir,
			RequestId:            createRequestID,
			ExpiresAtUnixSeconds: expiresAt, Metadata: metadata,
			FailurePolicy: failurePolicy, RecoveryTimeoutSeconds: recoveryTimeout,
			ActionBindings: apiBindings,
			Completion:     fastpathv2.CreateCompletion_CREATE_COMPLETION_READY,
		}
		klog.V(4).InfoS("sending CreateSandbox request", "name", name, "image", config.Image, "pool", config.PoolRef, "namespace", req.Namespace)

		resp, err := client.CreateSandbox(context.Background(), req)
		if err != nil {
			klog.ErrorS(err, "CreateSandbox request failed", "name", name)
			exitWithError(err)
		}

		info := resp.GetSandbox()
		if info == nil {
			exitWithErrorf("CreateSandbox returned no Sandbox observation")
		}
		// Cold images are delivered asynchronously: the create returns as
		// soon as the Sandbox is accepted (runtime Creating) and the
		// artifact delivery + boot finish in the background. Poll until the
		// Sandbox is Ready or reaches a terminal failure.
		if !info.GetReady() {
			namespace := viper.GetString("namespace")
			fmt.Printf("Sandbox %s accepted (runtime %s); waiting for image delivery and startup...\n", name, info.GetRuntime().GetState())
			info = waitForSandboxReady(context.Background(), client, name, namespace)
		}

		klog.V(4).InfoS("sandbox created successfully", "name", name, "sandboxUid", info.GetIdentity().GetUid(), "sandboxName", info.GetIdentity().GetName(), "ready", info.GetReady(), "duration", time.Since(start))
		fmt.Printf("🎉 Sandbox runtime created successfully in %v\n", time.Since(start))
		fmt.Printf("Name:      %s\n", info.GetIdentity().GetName())
		fmt.Printf("UID:       %s\n", info.GetIdentity().GetUid())
		fmt.Printf("Ready:     %t\n", info.GetReady())
	},
}

// waitForSandboxReady polls the Sandbox observation until it is Ready or
// reaches a terminal state. Cold creates park in runtime Creating while the
// image is delivered asynchronously, so the CLI resolves READY semantics on
// the client side instead of holding the create RPC open.
func waitForSandboxReady(ctx context.Context, client fastpathv2.FastPathServiceClient, name, namespace string) *fastpathv2.SandboxInfo {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		response, err := client.GetSandbox(ctx, &fastpathv2.GetSandboxRequest{
			Sandbox: fastPathSandboxReference(name, namespace),
		})
		if err != nil {
			klog.ErrorS(err, "GetSandbox while waiting for readiness failed", "name", name)
			exitWithError(err)
		}
		info := response.GetSandbox()
		if info == nil {
			exitWithErrorf("GetSandbox returned no Sandbox observation")
		}
		if info.GetReady() {
			return info
		}
		switch info.GetRuntime().GetState() {
		case fastpathv2.RuntimeState_RUNTIME_STATE_FAILED,
			fastpathv2.RuntimeState_RUNTIME_STATE_UNAVAILABLE,
			fastpathv2.RuntimeState_RUNTIME_STATE_STOPPED:
			exitWithErrorf("Sandbox %s reached terminal state %s", name, info.GetRuntime().GetState())
		}
		select {
		case <-ctx.Done():
			exitWithErrorf("waiting for Sandbox %s to become ready: %v", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(&configFile, "file", "f", "", "Path to sandbox config file")
	runCmd.Flags().StringVar(&image, "image", "", "Container image")
	runCmd.Flags().StringVar(&pool, "pool", "default-pool", "Target SandboxPool")
	runCmd.Flags().StringVar(&requestID, "request-id", "", "Idempotency key; must equal the Sandbox name")
	runCmd.Flags().Int64Var(&runExpiresAt, "expires-at", 0, "Absolute expiration time as a Unix timestamp")
	runCmd.Flags().StringSliceVar(&runMetadata, "metadata", nil, "Metadata to persist (key=value)")
	runCmd.Flags().StringVar(&runFailurePolicy, "failure-policy", "", "Failure policy (Manual|AutoRecreate)")
	runCmd.Flags().Int32Var(&runRecoveryTimeout, "recovery-timeout", 0, "Recovery delay in seconds")
	runCmd.Flags().StringArrayVar(&runActionBindings, "action", nil, "Ordered Action Binding as handler=opaque-input; repeat to add Bindings")
}

func parseActionBindings(values []string) ([]ActionBindingConfig, error) {
	result := make([]ActionBindingConfig, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || parts[0] == "" {
			return nil, fmt.Errorf("invalid Action Binding %q; expected handler=opaque-input", value)
		}
		if _, found := seen[parts[0]]; found {
			return nil, fmt.Errorf("duplicate Action Handler %q", parts[0])
		}
		seen[parts[0]] = struct{}{}
		result = append(result, ActionBindingConfig{Handler: parts[0], Input: parts[1]})
	}
	return result, nil
}

func runInteractive(name string, config *SandboxConfig) error {
	cacheDir := os.ExpandEnv("$HOME/.fastctl/cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache dir: %v", err)
	}
	cacheFile := cacheDir + "/" + name + ".yaml"

	var template string
	if cachedContent, err := os.ReadFile(cacheFile); err == nil {
		template = string(cachedContent)
		fmt.Printf("📋 Loading cached config for %s\n", name)
	} else {
		template = defaultTemplate(name)
		fmt.Printf("📋 Creating new sandbox: %s\n", name)
	}

	tmpFile, err := os.CreateTemp("", "fastctl-sandbox-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(template); err != nil {
		return fmt.Errorf("failed to write template: %v", err)
	}
	tmpFile.Close()

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}

	cmd := exec.Command(editor, tmpFile.Name())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Println("\n✅ Cancelled")
		return fmt.Errorf("cancelled by user")
	}

	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		return fmt.Errorf("failed to read config: %v", err)
	}

	if err := yaml.Unmarshal(content, config); err != nil {
		return fmt.Errorf("YAML parse error: %v\n  Hint: Fix the format and run again with the same name", err)
	}

	if config.Image == "" {
		return fmt.Errorf("invalid config: 'image' field is required")
	}

	fmt.Printf("\n创建 sandbox '%s'? (y/n): ", name)
	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "y" && confirm != "Y" {
		fmt.Println("✅ Cancelled")
		return fmt.Errorf("cancelled by user")
	}

	if err := os.WriteFile(cacheFile, content, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update cache: %v\n", err)
	}

	return nil
}

func defaultTemplate(name string) string {
	return fmt.Sprintf(`# fastctl sandbox configuration
# Name: %s (set via CLI argument)

# Container image to run (Required)
image: docker.io/library/alpine:latest

# Target SandboxPool (Required)
pool_ref: default-pool

# Optional: Override entrypoint and arguments
command: ["/bin/sleep", "3600"]
args: []

# Optional: Working directory
# working_dir: /app

# Optional: Environment variables
# envs:
#   KEY: value
`, name)
}
