package cmd

import (
	"context"
	"fmt"
	"log"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

var resumeCheckpointID string

var resumeCmd = &cobra.Command{
	Use:   "resume <sandbox-name>",
	Short: "Resume a paused sandbox from its checkpoint",
	Long: `Resume a paused sandbox: the controller schedules the recorded checkpoint
again (possibly on another Fastlet) and restores its memory under the same
Sandbox identity.

The call returns as soon as the desired state is persisted. Completion is
asynchronous: poll with "fastctl get <sandbox-name>" until the runtime state is
READY. Use --checkpoint-id to fence against a re-pause between the read and
this call.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		sandboxName := args[0]
		namespace := viper.GetString("namespace")
		klog.V(4).InfoS("CLI resume command started", "sandboxName", sandboxName, "namespace", namespace)

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		response, err := client.ResumeSandbox(context.Background(), &fastpathv2.ResumeSandboxRequest{
			Sandbox:              fastPathSandboxReference(sandboxName, namespace),
			ExpectedCheckpointId: resumeCheckpointID,
		})
		if err != nil {
			klog.ErrorS(err, "ResumeSandbox request failed", "sandboxName", sandboxName, "namespace", namespace)
			log.Fatalf("Error: %v", err)
		}
		fmt.Printf("Sandbox %s resume requested\n", sandboxName)
		fmt.Printf("  state: %s (watch status.runtime.state until READY)\n", response.GetSandbox().GetState())
	},
}

func init() {
	rootCmd.AddCommand(resumeCmd)
	resumeCmd.Flags().StringVar(&resumeCheckpointID, "checkpoint-id", "", "Expected checkpoint id (fences against a re-pause)")
}
