package cmd

import (
	"context"
	"fmt"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

var resetCmd = &cobra.Command{
	Use:     "reset <sandbox-name>",
	Aliases: []string{"restart"},
	Short:   "Reset/Restart a sandbox",
	Long: `Trigger a sandbox reset by updating its ResetRevision field.

This will cause the controller to reschedule the sandbox to a new fastlet pod,
preserving the sandbox configuration.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		sandboxName := args[0]
		namespace := viper.GetString("namespace")
		klog.V(4).InfoS("CLI reset command started", "sandboxName", sandboxName, "namespace", namespace)

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		resetRevision := time.Now().Format(time.RFC3339Nano)
		klog.V(4).InfoS("triggering sandbox reset", "sandboxName", sandboxName, "resetRevision", resetRevision)

		req := &fastpathv2.UpdateSandboxRequest{
			Sandbox: fastPathSandboxReference(sandboxName, namespace),
			Update: &fastpathv2.UpdateSandboxRequest_ResetRevision{
				ResetRevision: resetRevision,
			},
		}

		_, err := client.UpdateSandbox(context.Background(), req)
		if err != nil {
			klog.ErrorS(err, "UpdateSandbox request failed for reset", "sandboxName", sandboxName)
			exitWithError(err)
		}

		klog.V(4).InfoS("sandbox reset triggered successfully", "sandboxName", sandboxName)
		fmt.Printf("✓ Sandbox %s reset triggered\n", sandboxName)
		fmt.Printf("  The sandbox will be rescheduled to a new fastlet\n")
	},
}

func init() {
	rootCmd.AddCommand(resetCmd)
}
