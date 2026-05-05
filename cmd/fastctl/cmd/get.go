package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

var outputFormat string

var getCmd = &cobra.Command{
	Use:   "get <sandbox-name>",
	Short: "Get detailed sandbox information",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		sandboxName := args[0]
		namespace := viper.GetString("namespace")
		klog.V(4).InfoS("CLI get command started", "sandboxName", sandboxName, "namespace", namespace)

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		klog.V(4).InfoS("Sending GetSandbox request", "sandboxName", sandboxName, "namespace", namespace)
		resp, err := client.GetSandbox(context.Background(), &fastpathv1.GetRequest{
			SandboxName: sandboxName,
			Namespace:   namespace,
		})
		if err != nil {
			klog.ErrorS(err, "GetSandbox request failed", "sandboxName", sandboxName, "namespace", namespace)
			log.Fatalf("Error: %v", err)
		}

		klog.V(4).InfoS("GetSandbox request succeeded", "sandboxId", resp.SandboxId, "sandboxName", resp.SandboxName, "phase", resp.Phase, "outputFormat", outputFormat)
		if outputFormat == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(resp)
		} else {
			// Default YAML-like output
			y, _ := yaml.Marshal(resp)
			fmt.Print(string(y))
			fmt.Printf("Age: %s\n", time.Since(time.Unix(resp.CreatedAt, 0)).Round(time.Second))
		}
	},
}

func init() {
	rootCmd.AddCommand(getCmd)
	getCmd.Flags().StringVarP(&outputFormat, "output", "o", "yaml", "Output format (yaml|json)")
}
