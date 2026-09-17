package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

var (
	updateExpireTime      string
	updateFailurePolicy   string
	updateRecoveryTimeout int32
	updateMetadata        []string
	deleteMetadata        []string
	updateActionBindings  []string
	clearActionBindings   bool
)

var updateCmd = &cobra.Command{
	Use:   "update <sandbox-name>",
	Short: "Update sandbox configuration",
	Long: `Update sandbox properties such as expiration, failure policy, or metadata.

Examples:
  # Extend expiration to 1 hour from now
  fastctl update my-sandbox --expire-time $(($(date +%s) + 3600))

  # Remove expiration
  fastctl update my-sandbox --expire-time 0

  # Set failure policy to auto-recreate
  fastctl update my-sandbox --failure-policy AutoRecreate

  # Set user metadata
  fastctl update my-sandbox --metadata env=prod,tier=backend

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

		req := &fastpathv2.UpdateSandboxRequest{
			Sandbox:        fastPathSandboxReference(sandboxName, namespace),
			MetadataUpsert: make(map[string]string),
		}

		if cmd.Flags().Changed("expire-time") {
			seconds, err := parseExpireTime(updateExpireTime)
			if err != nil {
				klog.ErrorS(err, "Invalid expire-time value", "expireTime", updateExpireTime)
				exitWithErrorf("invalid expire-time: %v", err)
			}
			klog.V(4).InfoS("updating expire-time", "sandboxName", sandboxName, "expireTime", seconds)
			req.Update = &fastpathv2.UpdateSandboxRequest_ExpiresAtUnixSeconds{
				ExpiresAtUnixSeconds: seconds,
			}
		}

		if cmd.Flags().Changed("failure-policy") {
			policy, err := parseFailurePolicy(updateFailurePolicy)
			if err != nil {
				klog.ErrorS(err, "Invalid failure-policy value", "failurePolicy", updateFailurePolicy)
				exitWithErrorf("invalid failure-policy: %v", err)
			}
			klog.V(4).InfoS("updating failure-policy", "sandboxName", sandboxName, "failurePolicy", policy)
			req.Update = &fastpathv2.UpdateSandboxRequest_FailurePolicy{
				FailurePolicy: policy,
			}
		}

		if cmd.Flags().Changed("recovery-timeout") {
			klog.V(4).InfoS("updating recovery-timeout", "sandboxName", sandboxName, "recoveryTimeout", updateRecoveryTimeout)
			req.Update = &fastpathv2.UpdateSandboxRequest_RecoveryTimeoutSeconds{
				RecoveryTimeoutSeconds: updateRecoveryTimeout,
			}
		}

		if cmd.Flags().Changed("action") || clearActionBindings {
			if cmd.Flags().Changed("action") && clearActionBindings {
				exitWithErrorf("--action and --clear-actions cannot be used together")
			}
			bindings, err := parseActionBindings(updateActionBindings)
			if err != nil {
				exitWithError(err)
			}
			items := make([]*fastpathv2.ActionBinding, 0, len(bindings))
			for _, binding := range bindings {
				items = append(items, &fastpathv2.ActionBinding{Handler: binding.Handler, Input: binding.Input})
			}
			req.Update = &fastpathv2.UpdateSandboxRequest_ActionBindings{
				ActionBindings: &fastpathv2.ReplaceActionBindings{Items: items},
			}
		}

		if len(updateMetadata) > 0 {
			klog.V(4).InfoS("updating metadata", "sandboxName", sandboxName, "metadata", updateMetadata)
			for _, item := range updateMetadata {
				parts := strings.SplitN(item, "=", 2)
				if len(parts) != 2 {
					exitWithErrorf("invalid metadata format '%s', expected key=value", item)
				}
				req.MetadataUpsert[parts[0]] = parts[1]
			}
		}
		req.MetadataDeleteKeys = append(req.MetadataDeleteKeys, deleteMetadata...)

		if req.Update == nil && len(req.MetadataUpsert) == 0 && len(req.MetadataDeleteKeys) == 0 {
			klog.ErrorS(nil, "No update field specified")
			exitWithErrorf("at least one update field must be specified (--expire-time, --failure-policy, --recovery-timeout, --action, --clear-actions, --metadata, or --delete-metadata)")
		}

		klog.V(4).InfoS("sending UpdateSandbox request", "sandboxName", sandboxName)
		resp, err := client.UpdateSandbox(context.Background(), req)
		if err != nil {
			klog.ErrorS(err, "UpdateSandbox request failed", "sandboxName", sandboxName)
			exitWithError(err)
		}

		klog.V(4).InfoS("updateSandbox request succeeded", "sandboxName", sandboxName)
		fmt.Printf("✓ Sandbox %s update committed\n", sandboxName)
		fmt.Printf("  Committed generation: %d\n", resp.CommittedGeneration)
	},
}

func init() {
	rootCmd.AddCommand(updateCmd)

	updateCmd.Flags().StringVar(&updateExpireTime, "expire-time", "", "Expiration time (Unix timestamp or '0' to remove)")
	updateCmd.Flags().StringVar(&updateFailurePolicy, "failure-policy", "", "Failure policy (Manual|AutoRecreate)")
	updateCmd.Flags().Int32Var(&updateRecoveryTimeout, "recovery-timeout", 0, "Recovery timeout in seconds")
	updateCmd.Flags().StringSliceVar(&updateMetadata, "metadata", nil, "Metadata to set (key=value)")
	updateCmd.Flags().StringSliceVar(&deleteMetadata, "delete-metadata", nil, "Metadata keys to delete")
	updateCmd.Flags().StringArrayVar(&updateActionBindings, "action", nil, "Replace ordered Action Bindings with handler=opaque-input; repeat for multiple Bindings")
	updateCmd.Flags().BoolVar(&clearActionBindings, "clear-actions", false, "Replace the Action Binding list with an empty list")
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

func parseFailurePolicy(input string) (fastpathv2.FailurePolicy, error) {
	switch strings.ToLower(input) {
	case "manual":
		return fastpathv2.FailurePolicy_MANUAL, nil
	case "auto-recreate", "autorecreate", "auto":
		return fastpathv2.FailurePolicy_AUTO_RECREATE, nil
	default:
		return fastpathv2.FailurePolicy_MANUAL, fmt.Errorf("unknown failure policy: %s (valid: Manual, AutoRecreate)", input)
	}
}
