package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"fast-sandbox/pkg/sandboxclient"

	"github.com/spf13/cobra"
)

var (
	execStdin   bool
	execTimeout time.Duration
	execTTY     bool
)

var execCmd = &cobra.Command{
	Use:   "exec <sandbox-name> -- <command> [args...]",
	Short: "Execute a command through the injected Execd component",
	Long:  "Resolve the Sandbox data-plane route, then stream an OpenSandbox Execd command directly through Sandbox Proxy.",
	Args:  cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		if execStdin {
			log.Fatal("Error: --stdin requires an interactive Execd session adapter and is not supported by the /command adapter")
		}
		if execTTY {
			log.Fatal("Error: --tty requires the Execd PTY WebSocket extension and is not part of the configured adapter contract")
		}
		command, err := sandboxclient.ShellJoin(args[1:])
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		client, connection := getClient()
		if connection != nil {
			defer connection.Close()
		}
		adapter, err := newExecdAdapter(client)
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		execution, err := runExecdCommand(cmd.Context(), adapter, sandboxReference(args[0]), command, execTimeout)
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		if execution.Error != nil {
			fmt.Fprintf(os.Stderr, "%s: %s\n", execution.Error.Name, execution.Error.Value)
		}
		if execution.ExitCode != nil && *execution.ExitCode != 0 {
			os.Exit(*execution.ExitCode)
		}
	},
}

func runExecdCommand(ctx context.Context, adapter *sandboxclient.ExecdAdapter, sandbox sandboxclient.SandboxRef, command string, timeout time.Duration) (sandboxclient.Execution, error) {
	return adapter.RunCommand(ctx, sandbox, sandboxclient.RunCommandRequest{Command: command, Timeout: timeout}, &sandboxclient.ExecutionHandlers{
		OnStdout: func(message sandboxclient.OutputMessage) error {
			_, err := fmt.Fprint(os.Stdout, message.Text)
			return err
		},
		OnStderr: func(message sandboxclient.OutputMessage) error {
			_, err := fmt.Fprint(os.Stderr, message.Text)
			return err
		},
		SkipAccumulation: true,
	})
}

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().BoolVarP(&execStdin, "stdin", "i", false, "Read stdin (requires a session-capable adapter)")
	execCmd.Flags().DurationVar(&execTimeout, "timeout", 0, "Command timeout, for example 30s")
	execCmd.Flags().BoolVarP(&execTTY, "tty", "t", false, "Allocate a PTY (requires a PTY-capable adapter)")
}
