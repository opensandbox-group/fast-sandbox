package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Host conventions (ADR-002/003/004/009). Every path is overridable through a
// persistent flag so the tool can run against non-default deployments.
const (
	defaultUblkdSocket     = "/var/run/overlaybd-ublk/ublkd.sock"
	defaultCredFile        = "/opt/overlaybd/cred.json"
	defaultKernelDir       = "/opt/template-vm/kernels"
	defaultStateDir        = "/var/lib/template-vm/sandboxes"
	defaultRuntimeDir      = "/run/template-vm/sandboxes"
	defaultFirecrackerBin  = "firecracker"
	defaultOverlaybdCreate = "/opt/overlaybd/bin/overlaybd-create"
	defaultOverlaybdCommit = "/opt/overlaybd/bin/overlaybd-commit"
	defaultOverlaybdResize = "/opt/overlaybd/bin/overlaybd-resize"
	defaultConvertor       = "/opt/overlaybd/snapshotter/convertor"
	defaultBuildWorkDir    = "/var/lib/template-vm/builds"
	defaultBuildOutputDir  = "/var/lib/template-vm/artifacts"
)

// config holds the resolved global options shared by every subcommand. It is
// populated from the root command's persistent flags before RunE fires.
type config struct {
	ublkdSocket     string
	credFile        string
	kernelDir       string
	stateDir        string
	runtimeDir      string
	firecrackerBin  string
	overlaybdCreate string
	overlaybdCommit string
	overlaybdResize string
	convertor       string
	buildWorkDir    string
	buildOutputDir  string
	plainHTTP       bool
}

// global is the process-wide configuration bound to the persistent flags.
var global config

// sandboxHome returns the persistent state directory of a sandbox
// (<state-dir>/<id>/): upper data/index, config.v1.json files, the snapfiles
// mount point and state.json (ADR-009).
func (c *config) sandboxHome(id string) string {
	return filepath.Join(c.stateDir, id)
}

// sandboxRuntime returns the volatile runtime directory of a sandbox
// (<runtime-dir>/<id>/): the Firecracker API socket and the serial log.
func (c *config) sandboxRuntime(id string) string {
	return filepath.Join(c.runtimeDir, id)
}

var rootCmd = &cobra.Command{
	Use:   "template-vm",
	Short: "Restore a Firecracker microVM from a remote overlaybd template",
	Long: `template-vm restores a running microVM on the local host from a template
stored in a remote OCI registry (a pair of overlaybd images). It bypasses
every control plane and drives overlaybd-ublkd and Firecracker directly.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command and maps errors to a non-zero exit code.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func init() {
	flags := rootCmd.PersistentFlags()
	flags.StringVar(&global.ublkdSocket, "ublkd-socket", defaultUblkdSocket, "overlaybd-ublkd control socket")
	flags.StringVar(&global.credFile, "cred-file", defaultCredFile, "overlaybd global credential file")
	flags.StringVar(&global.kernelDir, "kernel-dir", defaultKernelDir, "directory holding vmlinux-<kernel_version> kernels")
	flags.StringVar(&global.stateDir, "state-dir", defaultStateDir, "persistent per-sandbox state directory root")
	flags.StringVar(&global.runtimeDir, "runtime-dir", defaultRuntimeDir, "volatile per-sandbox runtime directory root")
	flags.StringVar(&global.firecrackerBin, "firecracker", defaultFirecrackerBin, "firecracker binary path")
	flags.StringVar(&global.overlaybdCreate, "overlaybd-create", defaultOverlaybdCreate, "overlaybd-create binary path")
	flags.StringVar(&global.overlaybdCommit, "overlaybd-commit", defaultOverlaybdCommit, "overlaybd-commit binary path")
	flags.StringVar(&global.overlaybdResize, "overlaybd-resize", defaultOverlaybdResize, "overlaybd-resize binary path")
	flags.StringVar(&global.convertor, "overlaybd-convertor", defaultConvertor, "overlaybd userspace convertor binary path")
	flags.StringVar(&global.buildWorkDir, "build-work-dir", defaultBuildWorkDir, "temporary build directory root (build command)")
	flags.StringVar(&global.buildOutputDir, "build-output-dir", defaultBuildOutputDir, "artifact output directory root (build command)")
	flags.BoolVar(&global.plainHTTP, "plain-http", false, "use plain HTTP for registry requests (ORAS and overlaybd repoBlobUrl)")

	rootCmd.AddCommand(newCreateCommand())
	rootCmd.AddCommand(newListCommand())
	rootCmd.AddCommand(newDeleteCommand())
	rootCmd.AddCommand(newBuildCommand())
}
