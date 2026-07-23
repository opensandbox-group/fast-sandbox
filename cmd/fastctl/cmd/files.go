package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"fast-sandbox/pkg/sandboxclient"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/spf13/cobra"
)

var filesCmd = &cobra.Command{Use: "files", Short: "Manage files with the official OpenSandbox Execd SDK"}

var filesStatCmd = &cobra.Command{
	Use: "stat <sandbox-name> <path>", Short: "Read sandbox file metadata", Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		client, closeClient := commandOpenSandboxClient(cmd.Context(), args[0])
		defer closeClient()
		info, err := client.GetFileInfo(cmd.Context(), args[1])
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		entry, found := info[args[1]]
		if !found {
			log.Fatalf("Error: Execd response omitted file %q", args[1])
		}
		printJSON(entry)
	},
}

var filesListCmd = &cobra.Command{
	Use: "list <sandbox-name> <path>", Aliases: []string{"ls"}, Short: "List a sandbox directory", Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		client, closeClient := commandOpenSandboxClient(cmd.Context(), args[0])
		defer closeClient()
		entries, err := client.SearchFiles(cmd.Context(), args[1], "*")
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
	Run: func(cmd *cobra.Command, args []string) {
		client, closeClient := commandOpenSandboxClient(cmd.Context(), args[0])
		defer closeClient()
		download, err := client.DownloadFile(cmd.Context(), args[1], "")
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		defer download.Close()
		if _, err := io.Copy(os.Stdout, download); err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

var filesWriteCmd = &cobra.Command{
	Use: "write <sandbox-name> <path> [local-file]", Short: "Upload stdin or a local file into a sandbox", Args: cobra.RangeArgs(2, 3),
	Run: func(cmd *cobra.Command, args []string) {
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
		client, closeClient := commandOpenSandboxClient(cmd.Context(), args[0])
		defer closeClient()
		if err := client.UploadFile(cmd.Context(), source, opensandbox.UploadFileOptions{
			FileName: filepath.Base(args[1]),
			Metadata: opensandbox.FileMetadata{Path: args[1], Mode: opensandbox.OctalMode(0o644)},
		}); err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

var filesMkdirCmd = &cobra.Command{
	Use: "mkdir <sandbox-name> <path>", Short: "Create a sandbox directory and its parents", Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		client, closeClient := commandOpenSandboxClient(cmd.Context(), args[0])
		defer closeClient()
		if err := client.CreateDirectory(cmd.Context(), args[1], opensandbox.OctalMode(0o755)); err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

var removeDirectory bool

var filesRmCmd = &cobra.Command{
	Use: "rm <sandbox-name> <path>", Short: "Delete a sandbox file or directory", Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		client, closeClient := commandOpenSandboxClient(cmd.Context(), args[0])
		defer closeClient()
		var err error
		if removeDirectory {
			err = client.DeleteDirectory(cmd.Context(), args[1])
		} else {
			err = client.DeleteFiles(cmd.Context(), []string{args[1]})
		}
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

func commandOpenSandboxExecd() (*sandboxclient.OpenSandboxExecd, func()) {
	control, connection := getClient()
	return newOpenSandboxExecd(control), func() {
		if connection != nil {
			_ = connection.Close()
		}
	}
}

func commandOpenSandboxClient(ctx context.Context, sandboxName string) (*opensandbox.ExecdClient, func()) {
	adapter, closeControl := commandOpenSandboxExecd()
	client, _, err := adapter.Client(ctx, sandboxReference(sandboxName))
	if err != nil {
		closeControl()
		log.Fatalf("Error: %v", err)
	}
	return client, closeControl
}

func printJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

func init() {
	openSandboxCmd.AddCommand(filesCmd)
	filesCmd.AddCommand(filesStatCmd, filesListCmd, filesReadCmd, filesWriteCmd, filesMkdirCmd, filesRmCmd)
	filesRmCmd.Flags().BoolVarP(&removeDirectory, "directory", "r", false, "Delete a directory recursively instead of a file")
}
