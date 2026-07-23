package cmd

import "github.com/spf13/cobra"

var openSandboxCmd = &cobra.Command{
	Use:   "opensandbox",
	Short: "Use an injected OpenSandbox Execd component",
	Long:  "Resolve and authenticate a Fast Sandbox route, then delegate command and file operations to the official OpenSandbox Go SDK.",
}

func init() {
	rootCmd.AddCommand(openSandboxCmd)
}
