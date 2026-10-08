package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sandboxes in the state directory and verify liveness",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runList()
		},
	}
}

// runList enumerates the state directory and reports each sandbox's recorded
// devices plus its live/dead status derived from the firecracker pid
// (ADR-009).
func runList() error {
	ids, err := listSandboxIDs(global.stateDir)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "SANDBOX_ID\tSTATUS\tPID\tROOTFS_DEV\tSNAPFILES_DEV\tAPI_SOCKET")
	for _, id := range ids {
		state, err := readState(global.sandboxHome(id))
		if err != nil {
			fmt.Fprintf(writer, "%s\t%s\t-\t-\t-\t-\n", id, "corrupt")
			continue
		}
		status := "stopped"
		if processAlive(state.FirecrackerPID) {
			status = "running"
		}
		fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%s\t%s\n",
			state.SandboxID, status, state.FirecrackerPID,
			state.RootfsDevPath, state.SnapfilesDevPath, state.APISocket)
	}
	return writer.Flush()
}
