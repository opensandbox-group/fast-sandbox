package main

import (
	"fmt"
	"os"
	"strconv"
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

// runList enumerates the state directory and reports each sandbox's phase,
// recorded devices and liveness. A running sandbox whose recorded Firecracker
// identity no longer matches a live process is reported as stopped
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
		status := string(state.Phase)
		if state.Phase == phaseRunning && !state.Firecracker.alive() {
			status = "stopped"
		}
		pid := "-"
		if state.Firecracker != nil {
			pid = strconv.Itoa(state.Firecracker.PID)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			state.SandboxID, status, pid,
			state.Rootfs.path(), state.Snapfiles.path(), state.APISocket)
	}
	return writer.Flush()
}
