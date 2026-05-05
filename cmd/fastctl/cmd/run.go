package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

// SandboxConfig for yaml
type SandboxConfig struct {
	Image           string            `yaml:"image"`
	PoolRef         string            `yaml:"pool_ref"`
	ConsistencyMode string            `yaml:"consistency_mode"` // "fast" or "strong"
	Command         []string          `yaml:"command,omitempty"`
	Args            []string          `yaml:"args,omitempty"`
	ExposedPorts    []int32           `yaml:"exposed_ports,omitempty"`
	Envs            map[string]string `yaml:"envs,omitempty"`
	WorkingDir      string            `yaml:"working_dir,omitempty"`
}

var (
	configFile string
	pool       string
	mode       string
	ports      []int32
	image      string
)

// runCmd represents the run command
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
			PoolRef:         "default-pool",
			ConsistencyMode: "fast",
		}

		if configFile != "" {
			klog.V(4).InfoS("Loading config from file", "file", configFile)
			data, err := os.ReadFile(configFile)
			if err != nil {
				klog.ErrorS(err, "Failed to read config file", "file", configFile)
				log.Fatalf("Failed to read config file: %v", err)
			}
			if err := yaml.Unmarshal(data, &config); err != nil {
				klog.ErrorS(err, "Failed to parse config file", "file", configFile)
				log.Fatalf("Failed to parse config file: %v", err)
			}
		} else if image == "" {
			fmt.Println("Entering interactive mode...")
			if err := runInteractive(name, &config); err != nil {
				klog.ErrorS(err, "Interactive mode failed", "name", name)
				log.Fatalf("Interactive mode failed: %v", err)
			}
		}

		if image != "" {
			config.Image = image
		}
		if pool != "" && cmd.Flags().Changed("pool") {
			config.PoolRef = pool
		}
		if mode != "" && cmd.Flags().Changed("mode") {
			config.ConsistencyMode = mode
		}
		if len(ports) > 0 {
			config.ExposedPorts = ports
		}
		if len(args) > 1 {
			config.Command = args[1:]
		}
		if config.Image == "" {
			klog.ErrorS(nil, "Image is required but not provided", "name", name)
			log.Fatal("Error: image is required (via flag, file, or interactive mode)")
		}

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		consistency := fastpathv1.ConsistencyMode_FAST
		if config.ConsistencyMode == "strong" {
			consistency = fastpathv1.ConsistencyMode_STRONG
		}

		start := time.Now()
		req := &fastpathv1.CreateRequest{
			Name:            name,
			Image:           config.Image,
			PoolRef:         config.PoolRef,
			ExposedPorts:    config.ExposedPorts,
			Namespace:       viper.GetString("namespace"),
			ConsistencyMode: consistency,
			Command:         config.Command,
			Args:            config.Args,
			Envs:            config.Envs,
			WorkingDir:      config.WorkingDir,
		}
		klog.V(4).InfoS("Sending CreateSandbox request", "name", name, "image", config.Image, "pool", config.PoolRef, "namespace", req.Namespace)

		resp, err := client.CreateSandbox(context.Background(), req)
		if err != nil {
			klog.ErrorS(err, "CreateSandbox request failed", "name", name)
			log.Fatalf("Error: %v", err)
		}

		klog.V(4).InfoS("Sandbox created successfully", "name", name, "sandboxId", resp.SandboxId, "sandboxName", resp.SandboxName, "fastlet", resp.FastletPod, "duration", time.Since(start))
		fmt.Printf("🎉 Sandbox created successfully in %v\n", time.Since(start))
		fmt.Printf("Name:      %s\n", resp.SandboxName)
		fmt.Printf("ID:        %s\n", resp.SandboxId)
		fmt.Printf("Fastlet:     %s\n", resp.FastletPod)
		fmt.Printf("Endpoints: %v\n", resp.Endpoints)
	},
}

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(&configFile, "file", "f", "", "Path to sandbox config file")
	runCmd.Flags().StringVar(&image, "image", "", "Container image")
	runCmd.Flags().StringVar(&pool, "pool", "default-pool", "Target SandboxPool")
	runCmd.Flags().StringVar(&mode, "mode", "fast", "Consistency mode (fast/strong)")
	runCmd.Flags().Int32SliceVar(&ports, "ports", []int32{}, "Exposed ports")
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

# Consistency mode: 'fast' (fastlet-first) or 'strong' (crd-first)
consistency_mode: fast

# Optional: Override entrypoint and arguments
command: ["/bin/sleep", "3600"]
args: []

# Optional: Working directory
# working_dir: /app

# Optional: Expose ports
# exposed_ports:
#   - 8080

# Optional: Environment variables
# envs:
#   KEY: value
`, name)
}
