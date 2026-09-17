package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	fastpathv2 "fast-sandbox/api/proto/v2"

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

		klog.V(4).InfoS("sending GetSandbox request", "sandboxName", sandboxName, "namespace", namespace)
		resp, err := client.GetSandbox(context.Background(), &fastpathv2.GetSandboxRequest{
			Sandbox: fastPathSandboxReference(sandboxName, namespace),
		})
		if err != nil {
			klog.ErrorS(err, "GetSandbox request failed", "sandboxName", sandboxName, "namespace", namespace)
			exitWithError(err)
		}

		info := resp.GetSandbox()
		klog.V(4).InfoS("getSandbox request succeeded", "sandboxUid", info.GetIdentity().GetUid(), "sandboxName", info.GetIdentity().GetName(), "runtimeState", info.GetRuntime().GetState(), "dataPlaneState", info.GetDataPlane().GetState(), "outputFormat", outputFormat)
		if outputFormat == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(resp)
		} else {
			y, _ := yaml.Marshal(resp)
			fmt.Print(string(y))
		}
	},
}

func init() {
	rootCmd.AddCommand(getCmd)
	getCmd.Flags().StringVarP(&outputFormat, "output", "o", "yaml", "Output format (yaml|json)")
}
