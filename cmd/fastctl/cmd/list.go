package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"text/tabwriter"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"

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

		klog.V(4).InfoS("Sending ListSandboxes request", "namespace", namespace)
		resp, err := client.ListSandboxes(context.Background(), &fastpathv1.ListRequest{
			Namespace: namespace,
		})
		if err != nil {
			klog.ErrorS(err, "ListSandboxes request failed", "namespace", namespace)
			log.Fatalf("Error: %v", err)
		}

		klog.V(4).InfoS("ListSandboxes request succeeded", "namespace", namespace, "count", len(resp.Items))
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tID\tPHASE\tIMAGE\tFASTLET\tAGE")
		for _, item := range resp.Items {
			age := time.Since(time.Unix(item.CreatedAt, 0)).Truncate(time.Second)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", item.SandboxName, item.SandboxId, item.Phase, item.Image, item.FastletPod, age)
		}
		w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}
