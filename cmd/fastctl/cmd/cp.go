package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type sandboxPath struct {
	Sandbox string
	Path    string
}

var cpCmd = &cobra.Command{
	Use: "cp <src> <dst>", Short: "Copy one file between the local filesystem and an Execd-enabled sandbox", Args: cobra.ExactArgs(2),
	Run: func(_ *cobra.Command, args []string) {
		sourceRemote, sourceOK := parseSandboxPath(args[0])
		destinationRemote, destinationOK := parseSandboxPath(args[1])
		if sourceOK == destinationOK {
			log.Fatal("Error: exactly one side of cp must be sandbox:path")
		}
		adapter, closeClient := commandExecdAdapter()
		defer closeClient()
		ctx := context.Background()
		if sourceOK {
			output, err := os.Create(args[1])
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			defer output.Close()
			if _, err := adapter.Download(ctx, sandboxReference(sourceRemote.Sandbox), sourceRemote.Path, output); err != nil {
				log.Fatalf("Error: %v", err)
			}
			return
		}
		input, err := os.Open(args[0])
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		defer input.Close()
		if err := adapter.Upload(ctx, sandboxReference(destinationRemote.Sandbox), destinationRemote.Path, input, 0o644); err != nil {
			log.Fatalf("Error: %v", err)
		}
	},
}

func parseSandboxPath(value string) (sandboxPath, bool) {
	separator := strings.Index(value, ":")
	if separator <= 0 {
		return sandboxPath{}, false
	}
	sandbox, path := value[:separator], value[separator+1:]
	if sandbox == "" || path == "" {
		return sandboxPath{}, false
	}
	if !strings.HasPrefix(path, "/") {
		path = fmt.Sprintf("/%s", path)
	}
	return sandboxPath{Sandbox: sandbox, Path: path}, true
}

func init() { rootCmd.AddCommand(cpCmd) }
