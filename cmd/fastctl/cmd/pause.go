package cmd

import (
	"context"
	"fmt"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

var pauseCmd = &cobra.Command{
	Use:   "pause <sandbox-name>",
	Short: "Pause (checkpoint) a sandbox and release its runtime",
	Long: `Pause a running sandbox: the controller checkpoints its memory and rootfs to
the artifact store, releases the Fastlet runtime, and keeps the Sandbox object
(same name/UID) as the resume ticket.

The call returns as soon as the desired state is persisted. Completion is
asynchronous: poll with "fastctl get <sandbox-name>" until the runtime state is
PAUSED and status.runtime.checkpoint is populated.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		sandboxName := args[0]
		namespace := viper.GetString("namespace")
		klog.V(4).InfoS("CLI pause command started", "sandboxName", sandboxName, "namespace", namespace)

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		response, err := client.PauseSandbox(context.Background(), &fastpathv2.PauseSandboxRequest{
			Sandbox: fastPathSandboxReference(sandboxName, namespace),
		})
		if err != nil {
			klog.ErrorS(err, "PauseSandbox request failed", "sandboxName", sandboxName, "namespace", namespace)
			exitWithError(err)
		}
		fmt.Printf("Sandbox %s pause requested\n", sandboxName)
		fmt.Printf("  state: %s (watch status.runtime.state until PAUSED)\n", response.GetSandbox().GetState())
	},
}

func init() {
	rootCmd.AddCommand(pauseCmd)
}
