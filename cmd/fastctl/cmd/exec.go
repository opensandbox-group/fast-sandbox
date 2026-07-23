package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"fast-sandbox/pkg/sandboxclient"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/spf13/cobra"
)

var (
	execStdin   bool
	execTimeout time.Duration
	execTTY     bool
)

var execCmd = &cobra.Command{
	Use:   "exec <sandbox-name> -- <command> [args...]",
	Short: "Execute a command with the official OpenSandbox Execd SDK",
	Args:  cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		if execStdin {
			log.Fatal("Error: --stdin requires an interactive Execd session and is not supported by this command")
		}
		if execTTY {
			log.Fatal("Error: --tty requires the Execd PTY extension and is not supported by this command")
		}
		command, err := sandboxclient.ShellJoin(args[1:])
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		control, connection := getClient()
		if connection != nil {
			defer connection.Close()
		}
		result, err := runOpenSandboxCommand(cmd.Context(), newOpenSandboxExecd(control), sandboxReference(args[0]), command, execTimeout)
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		if result.ErrorName != "" {
			fmt.Fprintf(os.Stderr, "%s: %s\n", result.ErrorName, result.ErrorValue)
		}
		if result.ExitCode != nil && *result.ExitCode != 0 {
			os.Exit(*result.ExitCode)
		}
	},
}

type commandResult struct {
	ExitCode   *int
	ErrorName  string
	ErrorValue string
}

func runOpenSandboxCommand(ctx context.Context, adapter *sandboxclient.OpenSandboxExecd, sandbox sandboxclient.SandboxRef, command string, timeout time.Duration) (commandResult, error) {
	client, _, err := adapter.Client(ctx, sandbox)
	if err != nil {
		return commandResult{}, err
	}
	request := opensandbox.RunCommandRequest{Command: command}
	if timeout > 0 {
		request.Timeout = timeout.Milliseconds()
	}
	var result commandResult
	err = client.RunCommand(ctx, request, func(event opensandbox.StreamEvent) error {
		var payload struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ExitCode *int   `json:"exit_code,omitempty"`
			EName    string `json:"ename,omitempty"`
			EValue   string `json:"evalue,omitempty"`
			Error    *struct {
				EName  string `json:"ename,omitempty"`
				EValue string `json:"evalue,omitempty"`
			} `json:"error,omitempty"`
		}
		if json.Unmarshal([]byte(event.Data), &payload) != nil {
			_, writeErr := fmt.Fprint(os.Stdout, event.Data)
			return writeErr
		}
		if payload.Type == "" {
			payload.Type = event.Event
		}
		switch payload.Type {
		case "stdout":
			_, writeErr := fmt.Fprint(os.Stdout, payload.Text)
			return writeErr
		case "stderr":
			_, writeErr := fmt.Fprint(os.Stderr, payload.Text)
			return writeErr
		case "error":
			result.ErrorName, result.ErrorValue = payload.EName, payload.EValue
			if payload.Error != nil {
				result.ErrorName, result.ErrorValue = payload.Error.EName, payload.Error.EValue
			}
			result.ExitCode = payload.ExitCode
			if result.ExitCode == nil {
				if code, parseErr := strconv.Atoi(result.ErrorValue); parseErr == nil {
					result.ExitCode = &code
				}
			}
		case "execution_complete":
			result.ExitCode = payload.ExitCode
			if result.ExitCode == nil && result.ErrorName == "" {
				zero := 0
				result.ExitCode = &zero
			}
		}
		return nil
	})
	return result, err
}

func init() {
	openSandboxCmd.AddCommand(execCmd)
	execCmd.Flags().BoolVarP(&execStdin, "stdin", "i", false, "Read stdin (requires a session-capable adapter)")
	execCmd.Flags().DurationVar(&execTimeout, "timeout", 0, "Command timeout, for example 30s")
	execCmd.Flags().BoolVarP(&execTTY, "tty", "t", false, "Allocate a PTY (requires the Execd PTY extension)")
}
