package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var diagnosticsCmd = &cobra.Command{
	Use:   "diagnostics",
	Short: "Inspect Fast Sandbox platform diagnostics",
}

var (
	diagnosticsLimit  int32
	diagnosticsOutput string
)

var diagnosticsSandboxCmd = &cobra.Command{
	Use:   "sandbox <sandbox-name>",
	Short: "Show CRD state and Fastlet lifecycle events (not process stdout/stderr)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, connection := getClient()
		if connection != nil {
			defer connection.Close()
		}
		response, err := fetchSandboxDiagnostics(cmd.Context(), client, args[0], viper.GetString("namespace"), diagnosticsLimit)
		if err != nil {
			return err
		}
		return printSandboxDiagnostics(os.Stdout, response, diagnosticsOutput)
	},
}

func fetchSandboxDiagnostics(ctx context.Context, client fastpathv1.FastPathServiceClient, name, namespace string, limit int32) (*fastpathv1.SandboxDiagnosticsResponse, error) {
	return client.GetSandboxDiagnostics(ctx, &fastpathv1.SandboxDiagnosticsRequest{
		SandboxName: name, Namespace: namespace, Limit: limit,
	})
}

func printSandboxDiagnostics(writer io.Writer, response *fastpathv1.SandboxDiagnosticsResponse, output string) error {
	if response == nil {
		return fmt.Errorf("diagnostics response is empty")
	}
	switch strings.ToLower(output) {
	case "json":
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(response)
	case "", "text":
	default:
		return fmt.Errorf("unsupported output %q; use text or json", output)
	}

	if response.Sandbox != nil {
		fmt.Fprintf(writer, "Sandbox:   %s (%s)\n", response.Sandbox.SandboxName, response.Sandbox.SandboxUid)
		fmt.Fprintf(writer, "States:    runtime=%s data-plane=%s user-process=%s\n", response.Sandbox.RuntimeState, response.Sandbox.DataPlaneState, response.Sandbox.UserProcessState)
		fmt.Fprintf(writer, "Fastlet:   %s\n", response.Sandbox.FastletPod)
	}
	fmt.Fprintf(writer, "Assignment: %s attempt=%d runtime-instance=%s\n", response.AssignmentState, response.AssignmentAttempt, response.RuntimeInstanceId)
	if response.FastletReachable {
		fmt.Fprintln(writer, "Fastlet diagnostics: reachable")
	} else {
		fmt.Fprintln(writer, "Fastlet diagnostics: unavailable")
	}
	if response.FastletError != "" {
		fmt.Fprintf(writer, "Diagnostic warning: %s\n", response.FastletError)
	}
	if len(response.Events) == 0 {
		fmt.Fprintln(writer, "Events: none retained")
		return nil
	}

	fmt.Fprintln(writer, "Events:")
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "TIME\tLEVEL\tSOURCE\tPHASE\tMESSAGE")
	for _, event := range response.Events {
		timestamp := time.Unix(0, event.TimestampUnixNano).UTC().Format(time.RFC3339Nano)
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", timestamp, event.Level, event.Source, event.Phase, event.Message)
	}
	return table.Flush()
}

func init() {
	rootCmd.AddCommand(diagnosticsCmd)
	diagnosticsCmd.AddCommand(diagnosticsSandboxCmd)
	diagnosticsSandboxCmd.Flags().Int32Var(&diagnosticsLimit, "limit", 50, "Maximum retained Fastlet events to return (1-128)")
	diagnosticsSandboxCmd.Flags().StringVarP(&diagnosticsOutput, "output", "o", "text", "Output format: text or json")
}
