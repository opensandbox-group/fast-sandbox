package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all sandboxes",
	Run: func(cmd *cobra.Command, args []string) {
		namespace := viper.GetString("namespace")
		klog.V(4).InfoS("CLI list command started", "namespace", namespace)

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		klog.V(4).InfoS("sending ListSandboxes request", "namespace", namespace)
		resp, err := client.ListSandboxes(context.Background(), &fastpathv2.ListSandboxesRequest{
			Namespace: namespace,
		})
		if err != nil {
			klog.ErrorS(err, "ListSandboxes request failed", "namespace", namespace)
			exitWithError(err)
		}

		klog.V(4).InfoS("listSandboxes request succeeded", "namespace", namespace, "count", len(resp.Items))
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tUID\tNAMESPACE\tGENERATION")
		for _, item := range resp.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", item.GetIdentity().GetName(), item.GetIdentity().GetUid(), item.GetIdentity().GetNamespace(), item.GetGeneration())
		}
		w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}
