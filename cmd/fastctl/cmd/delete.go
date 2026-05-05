package cmd

import (
	"context"
	"fmt"
	"log"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

var deleteCmd = &cobra.Command{
	Use:     "delete <sandbox-name>",
	Aliases: []string{"rm"},
	Short:   "Delete a sandbox",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		sandboxName := args[0]
		namespace := viper.GetString("namespace")
		klog.V(4).InfoS("CLI delete command started", "sandboxName", sandboxName, "namespace", namespace)

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		klog.V(4).InfoS("Sending DeleteSandbox request", "sandboxName", sandboxName, "namespace", namespace)
		_, err := client.DeleteSandbox(context.Background(), &fastpathv1.DeleteRequest{
			SandboxName: sandboxName,
			Namespace:   namespace,
		})
		if err != nil {
			klog.ErrorS(err, "DeleteSandbox request failed", "sandboxName", sandboxName, "namespace", namespace)
			log.Fatalf("Error: %v", err)
		}

		klog.V(4).InfoS("DeleteSandbox request succeeded", "sandboxName", sandboxName)
		fmt.Printf("Sandbox %s deletion triggered\n", sandboxName)
	},
}

func init() {
	rootCmd.AddCommand(deleteCmd)
}
