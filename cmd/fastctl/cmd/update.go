package cmd

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

var (
	updateExpireTime      string
	updateFailurePolicy   string
	updateRecoveryTimeout int32
	updateLabels          []string
)

// updateCmd represents the update command
var updateCmd = &cobra.Command{
	Use:   "update <sandbox-name>",
	Short: "Update sandbox configuration",
	Long: `Update sandbox properties such as expire time, failure policy, or labels.

Examples:
  # Extend expiration to 1 hour from now
  fastctl update my-sandbox --expire-time $(($(date +%s) + 3600))

  # Remove expiration
  fastctl update my-sandbox --expire-time 0

  # Set failure policy to auto-recreate
  fastctl update my-sandbox --failure-policy AutoRecreate

  # Add labels
  fastctl update my-sandbox --labels env=prod,tier=backend

  # Update recovery timeout
  fastctl update my-sandbox --recovery-timeout 120`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		sandboxName := args[0]
		namespace := viper.GetString("namespace")
		klog.V(4).InfoS("CLI update command started", "sandboxName", sandboxName, "namespace", namespace)

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		req := &fastpathv1.UpdateRequest{
			SandboxName: sandboxName,
			Namespace:   namespace,
			Labels:      make(map[string]string),
		}

		if cmd.Flags().Changed("expire-time") {
			seconds, err := parseExpireTime(updateExpireTime)
			if err != nil {
				klog.ErrorS(err, "Invalid expire-time value", "expireTime", updateExpireTime)
				log.Fatalf("Error: invalid expire-time: %v", err)
			}
			klog.V(4).InfoS("Updating expire-time", "sandboxName", sandboxName, "expireTime", seconds)
			req.Update = &fastpathv1.UpdateRequest_ExpireTimeSeconds{
				ExpireTimeSeconds: seconds,
			}
		}

		if cmd.Flags().Changed("failure-policy") {
			policy, err := parseFailurePolicy(updateFailurePolicy)
			if err != nil {
				klog.ErrorS(err, "Invalid failure-policy value", "failurePolicy", updateFailurePolicy)
				log.Fatalf("Error: invalid failure-policy: %v", err)
			}
			klog.V(4).InfoS("Updating failure-policy", "sandboxName", sandboxName, "failurePolicy", policy)
			req.Update = &fastpathv1.UpdateRequest_FailurePolicy{
				FailurePolicy: policy,
			}
		}

		if cmd.Flags().Changed("recovery-timeout") {
			klog.V(4).InfoS("Updating recovery-timeout", "sandboxName", sandboxName, "recoveryTimeout", updateRecoveryTimeout)
			req.Update = &fastpathv1.UpdateRequest_RecoveryTimeoutSeconds{
				RecoveryTimeoutSeconds: updateRecoveryTimeout,
			}
		}

		if len(updateLabels) > 0 {
			klog.V(4).InfoS("Updating labels", "sandboxName", sandboxName, "labels", updateLabels)
			for _, label := range updateLabels {
				parts := strings.SplitN(label, "=", 2)
				if len(parts) != 2 {
					klog.ErrorS(nil, "Invalid label format", "label", label)
					log.Fatalf("Error: invalid label format '%s', expected key=value", label)
				}
				req.Labels[parts[0]] = parts[1]
			}
		}

		if req.Update == nil && len(req.Labels) == 0 {
			klog.ErrorS(nil, "No update field specified")
			log.Fatal("Error: at least one update field must be specified (--expire-time, --failure-policy, --recovery-timeout, or --labels)")
		}

		klog.V(4).InfoS("Sending UpdateSandbox request", "sandboxName", sandboxName)
		resp, err := client.UpdateSandbox(context.Background(), req)
		if err != nil {
			klog.ErrorS(err, "UpdateSandbox request failed", "sandboxName", sandboxName)
			log.Fatalf("Error: %v", err)
		}

		if !resp.Success {
			klog.ErrorS(nil, "UpdateSandbox request returned failure", "sandboxName", sandboxName, "message", resp.Message)
			log.Fatalf("Error: %s", resp.Message)
		}

		klog.V(4).InfoS("UpdateSandbox request succeeded", "sandboxName", sandboxName)
		fmt.Printf("✓ Sandbox %s updated successfully\n", sandboxName)
		if resp.Sandbox != nil {
			fmt.Printf("  Runtime: %s\n", resp.Sandbox.RuntimeState)
			fmt.Printf("  Data plane: %s\n", resp.Sandbox.DataPlaneState)
			fmt.Printf("  Fastlet: %s\n", resp.Sandbox.FastletPod)
		}
	},
}

func init() {
	rootCmd.AddCommand(updateCmd)

	updateCmd.Flags().StringVar(&updateExpireTime, "expire-time", "", "Expiration time (Unix timestamp or '0' to remove)")
	updateCmd.Flags().StringVar(&updateFailurePolicy, "failure-policy", "", "Failure policy (Manual|AutoRecreate)")
	updateCmd.Flags().Int32Var(&updateRecoveryTimeout, "recovery-timeout", 0, "Recovery timeout in seconds")
	updateCmd.Flags().StringSliceVar(&updateLabels, "labels", []string{}, "Labels to set (key=value format)")
}

func parseExpireTime(input string) (int64, error) {
	if input == "0" {
		return 0, nil
	}

	seconds, err := strconv.ParseInt(input, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp format: %w", err)
	}

	return seconds, nil
}

func parseFailurePolicy(input string) (fastpathv1.FailurePolicy, error) {
	switch strings.ToLower(input) {
	case "manual":
		return fastpathv1.FailurePolicy_MANUAL, nil
	case "auto-recreate", "autorecreate", "auto":
		return fastpathv1.FailurePolicy_AUTO_RECREATE, nil
	default:
		return fastpathv1.FailurePolicy_MANUAL, fmt.Errorf("unknown failure policy: %s (valid: Manual, AutoRecreate)", input)
	}
}
