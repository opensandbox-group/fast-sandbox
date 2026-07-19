package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"fast-sandbox/pkg/sandboxclient"

	"github.com/spf13/cobra"
)

var filesCmd = &cobra.Command{Use: "files", Short: "Manage files through the injected Execd component"}

var filesStatCmd = &cobra.Command{
	Use: "stat <sandbox-name> <path>", Short: "Read sandbox file metadata", Args: cobra.ExactArgs(2),
	Run: func(_ *cobra.Command, args []string) {
		adapter, closeClient := commandExecdAdapter()
		defer closeClient()
		info, err := adapter.Stat(context.Background(), sandboxReference(args[0]), args[1])
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		printJSON(info)
	},
}

var filesListCmd = &cobra.Command{
	Use: "list <sandbox-name> <path>", Aliases: []string{"ls"}, Short: "List a sandbox directory", Args: cobra.ExactArgs(2),
	Run: func(_ *cobra.Command, args []string) {
		adapter, closeClient := commandExecdAdapter()
		defer closeClient()
		entries, err := adapter.List(context.Background(), sandboxReference(args[0]), args[1])
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		for _, entry := range entries {
			fmt.Println(entry.Path)
		}
	},
}

var filesReadCmd = &cobra.Command{
	Use: "read <sandbox-name> <path>", Short: "Stream a sandbox file to stdout", Args: cobra.ExactArgs(2),
	Run: func(_ *cobra.Command, args []string) {
		adapter, closeClient := commandExecdAdapter()
		defer closeClient()
		if _, err := adapter.Download(context.Background(), sandboxReference(args[0]), args[1], os.Stdout); err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

var filesWriteCmd = &cobra.Command{
	Use: "write <sandbox-name> <path> [local-file]", Short: "Upload stdin or a local file into a sandbox", Args: cobra.RangeArgs(2, 3),
	Run: func(_ *cobra.Command, args []string) {
		var source io.Reader = os.Stdin
		var input *os.File
		var err error
		if len(args) == 3 {
			input, err = os.Open(args[2])
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			defer input.Close()
			source = input
		}
		adapter, closeClient := commandExecdAdapter()
		defer closeClient()
		if err := adapter.Upload(context.Background(), sandboxReference(args[0]), args[1], source, 0o644); err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

var filesMkdirCmd = &cobra.Command{
	Use: "mkdir <sandbox-name> <path>", Short: "Create a sandbox directory and its parents", Args: cobra.ExactArgs(2),
	Run: func(_ *cobra.Command, args []string) {
		adapter, closeClient := commandExecdAdapter()
		defer closeClient()
		if err := adapter.MakeDir(context.Background(), sandboxReference(args[0]), args[1], 0o755); err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

var removeDirectory bool

var filesRmCmd = &cobra.Command{
	Use: "rm <sandbox-name> <path>", Short: "Delete a sandbox file or directory", Args: cobra.ExactArgs(2),
	Run: func(_ *cobra.Command, args []string) {
		adapter, closeClient := commandExecdAdapter()
		defer closeClient()
		if err := adapter.Delete(context.Background(), sandboxReference(args[0]), args[1], removeDirectory); err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

func commandExecdAdapter() (*sandboxclient.ExecdAdapter, func()) {
	client, connection := getClient()
	adapter, err := newExecdAdapter(client)
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		log.Fatalf("Error: %v", err)
	}
	return adapter, func() {
		if connection != nil {
			_ = connection.Close()
		}
	}
}

func printJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

func init() {
	rootCmd.AddCommand(filesCmd)
	filesCmd.AddCommand(filesStatCmd, filesListCmd, filesReadCmd, filesWriteCmd, filesMkdirCmd, filesRmCmd)
	filesRmCmd.Flags().BoolVarP(&removeDirectory, "directory", "r", false, "Delete a directory recursively instead of a file")
}
