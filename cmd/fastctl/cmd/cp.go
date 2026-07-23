package cmd

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
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
		adapter, closeClient := commandOpenSandboxExecd()
		defer closeClient()
		ctx := context.Background()
		if sourceOK {
			client, _, err := adapter.Client(ctx, sandboxReference(sourceRemote.Sandbox))
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			output, err := os.Create(args[1])
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			defer output.Close()
			download, err := client.DownloadFile(ctx, sourceRemote.Path, "")
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			defer download.Close()
			if _, err := io.Copy(output, download); err != nil {
				log.Fatalf("Error: %v", err)
			}
			return
		}
		input, err := os.Open(args[0])
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		defer input.Close()
		client, _, err := adapter.Client(ctx, sandboxReference(destinationRemote.Sandbox))
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		if err := client.UploadFile(ctx, input, opensandbox.UploadFileOptions{
			FileName: filepath.Base(destinationRemote.Path),
			Metadata: opensandbox.FileMetadata{Path: destinationRemote.Path, Mode: opensandbox.OctalMode(0o644)},
		}); err != nil {
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

func init() { openSandboxCmd.AddCommand(cpCmd) }
