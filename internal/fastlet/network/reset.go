package network

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Deep reset for pooled slot returns, its ordered steps, and the guards that
// keep captured values out of a shell unless they are known-safe.

func (d *LinuxNetNSDriver) DeepReset(ctx context.Context, slot *Slot, golden *GoldenState) error {
	return runDeepReset(ctx, d.runner, slot, golden, nil)
}

// runDeepReset captures the current namespace state, builds the ordered
// reset from it, appends the driver's own reset commands, and executes the
// whole reset as one in-namespace shell before flushing the kernel-internal
// conntrack domain.
func runDeepReset(ctx context.Context, runner CommandRunner, slot *Slot, golden *GoldenState, extraLines []string) error {
	if slot == nil || slot.NetNSName == "" || golden == nil {
		return fmt.Errorf("incomplete deep reset request")
	}
	current, err := captureNetState(ctx, runner, slot.NetNSName)
	if err != nil {
		return err
	}
	script, err := buildResetScript(current, golden)
	if err != nil {
		return err
	}
	if len(extraLines) > 0 {
		script += strings.Join(extraLines, "\n") + "\n"
	}
	if _, err := runner.Run(ctx, "ip", "netns", "exec", slot.NetNSName, "/bin/sh", "-c", script); err != nil {
		return fmt.Errorf("run deep reset: %w", err)
	}
	if err := conntrackFlush(slot.NetNSPath); err != nil {
		return fmt.Errorf("flush conntrack table: %w", err)
	}
	return nil
}

// validateDeviceName guards the reset script: device names are interpolated
// into a shell command line.
var validateDeviceName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// validateMAC guards MAC values interpolated into the reset script.
var validateMAC = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// validateSysctlKey guards sysctl key paths.
var validateSysctlKey = regexp.MustCompile(`^net\.[a-z0-9_.-]+$`)

// validateSysctlValue guards sysctl values; kernel-reported values are
// numeric or textual lists, and anything else must not reach a shell.
var validateSysctlValue = regexp.MustCompile(`^[0-9A-Za-z _.@:/,\t-]*$`)

// validateRouteField guards textual route selectors (tables, scopes,
// protocols, interface names, destinations).
var validateRouteField = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{0,64}$`)

// Reset steps are ordered and split by domain so each step stays small; the
// order in buildResetScript is the only safe one:
//
//   - links are quiesced before their queues are replaced, so a wedged qdisc
//     drains under the caller's reset timeout;
//   - the MAC is restored before neighbour entries are reinstated, because
//     setting a link address flushes the neighbour table;
//   - addresses are restored before links come up, so the kernel derives its
//     connected routes with the interface already configured;
//   - routes are reinstated after the links are up, because a route through a
//     down device is rejected by the kernel;
//   - sysctls are restored last; net.* sysctls are inherited from the
//     creating namespace rather than reset by the kernel, so a tenant-written
//     value survives a namespace without this step.
//
// Deleting address, neighbour, and route residue is idempotent best-effort:
// the audit that follows the reset is the authoritative gate, so deleting an
// entry that a prior step already removed must not fail the reset.
var resetSteps = []struct {
	name string
	run  func(current *netState, golden *GoldenState) ([]string, error)
}{
	{"quiesce", resetQuiesceLines},
	{"identity", resetIdentityLines},
	{"qdisc", resetQdiscLines},
	{"address", resetAddressLines},
	{"link-up", resetLinkUpLines},
	{"route-residue", resetRouteResidueLines},
	{"route-restore", resetRouteRestoreLines},
	{"neighbour", resetNeighbourLines},
	{"sysctl", resetSysctlLines},
}

func buildResetScript(current *netState, golden *GoldenState) (string, error) {
	// The golden state drives every restore command; it is validated as a
	// whole, not only for the entries a particular reset happens to use, so
	// an untrustworthy golden state can never reach a shell.
	if err := validateGoldenState(golden); err != nil {
		return "", err
	}
	lines := []string{"set -e"}
	for _, step := range resetSteps {
		stepLines, err := step.run(current, golden)
		if err != nil {
			return "", fmt.Errorf("build %s reset step: %w", step.name, err)
		}
		lines = append(lines, stepLines...)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func checkedDevice(name string) (string, error) {
	if !validateDeviceName.MatchString(name) {
		return "", fmt.Errorf("invalid device name %q in state", name)
	}
	return name, nil
}

func resetQuiesceLines(current *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, link := range golden.Links {
		device, err := checkedDevice(link.Name)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "ip link set dev "+device+" down")
	}
	for _, address := range current.Addresses {
		if addressInState(golden.Addresses, address) {
			continue
		}
		device, err := checkedDevice(address.Link)
		if err != nil {
			return nil, err
		}
		if _, err := parseCIDROrError(address.Address); err != nil {
			return nil, err
		}
		lines = append(lines, "ip addr del "+address.Address+" dev "+device+" || true")
	}
	for _, link := range golden.Links {
		device, err := checkedDevice(link.Name)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "ip neigh flush dev "+device+" || true")
	}
	return lines, nil
}

func resetIdentityLines(_ *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, link := range golden.Links {
		if link.Type == "loopback" || link.MAC == "" {
			continue
		}
		device, err := checkedDevice(link.Name)
		if err != nil {
			return nil, err
		}
		if !validateMAC.MatchString(link.MAC) {
			return nil, fmt.Errorf("invalid MAC %q for link %s in golden state", link.MAC, link.Name)
		}
		lines = append(lines, "ip link set dev "+device+" address "+link.MAC)
	}
	return lines, nil
}

func resetQdiscLines(current *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, qdisc := range current.Qdiscs {
		if qdiscInGolden(golden.Qdiscs, qdisc) {
			continue
		}
		device, err := checkedDevice(qdisc.Device)
		if err != nil {
			return nil, err
		}
		if qdisc.Root {
			lines = append(lines, "tc qdisc del dev "+device+" root || true")
			continue
		}
		if !validateRouteField.MatchString(qdisc.Parent) {
			return nil, fmt.Errorf("invalid qdisc parent %q in captured state", qdisc.Parent)
		}
		lines = append(lines, "tc qdisc del dev "+device+" parent "+qdisc.Parent+" || true")
	}
	return lines, nil
}

func resetAddressLines(current *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, address := range golden.Addresses {
		// Present addresses are deliberately left alone: re-touching an
		// address makes the kernel reinstall its derived main-table route
		// with a different metric, which is benign churn the audit must not
		// flag. Addresses survive the reset's own link transitions, so the
		// pre-reset snapshot is reliable here.
		if addressInState(current.Addresses, address) {
			continue
		}
		device, err := checkedDevice(address.Link)
		if err != nil {
			return nil, err
		}
		if _, err := parseCIDROrError(address.Address); err != nil {
			return nil, err
		}
		lines = append(lines, "ip addr replace "+address.Address+" dev "+device)
	}
	return lines, nil
}

func resetLinkUpLines(_ *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, link := range golden.Links {
		device, err := checkedDevice(link.Name)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "ip link set dev "+device+" up")
	}
	return lines, nil
}

func resetRouteResidueLines(current *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, route := range current.Routes {
		if routeInState(golden.Routes, route) {
			continue
		}
		command, err := routeCommand("del", route, false)
		if err != nil {
			return nil, err
		}
		lines = append(lines, command+" || true")
	}
	return lines, nil
}

func resetRouteRestoreLines(_ *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, route := range golden.Routes {
		// Kernel-protocol routes are derived state and reappear with their
		// addresses.
		if route.Protocol == "kernel" {
			continue
		}
		// replace is idempotent and the kernel drops routes of a link that
		// the reset transitions, so a route present in the pre-reset
		// snapshot may still need reinstatement.
		command, err := routeCommand("replace", route, true)
		if err != nil {
			return nil, err
		}
		lines = append(lines, command)
	}
	return lines, nil
}

func resetNeighbourLines(_ *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, neighbour := range golden.Neighbours {
		if !neighbour.Permanent {
			continue
		}
		device, err := checkedDevice(neighbour.Device)
		if err != nil {
			return nil, err
		}
		if !validateMAC.MatchString(neighbour.MAC) {
			return nil, fmt.Errorf("invalid neighbour MAC %q in golden state", neighbour.MAC)
		}
		if !validateRouteField.MatchString(neighbour.Destination) {
			return nil, fmt.Errorf("invalid neighbour destination %q in golden state", neighbour.Destination)
		}
		lines = append(lines, "ip neigh replace "+neighbour.Destination+" lladdr "+neighbour.MAC+" dev "+device+" nud permanent")
	}
	return lines, nil
}

func resetSysctlLines(current *netState, golden *GoldenState) ([]string, error) {
	var lines []string
	for _, key := range sortedKeys(golden.Sysctls) {
		if _, volatile := volatileNetSysctls[key]; volatile {
			continue
		}
		value, exists := current.Sysctls[key]
		if !exists || value == golden.Sysctls[key] {
			continue
		}
		if !validateSysctlKey.MatchString(key) {
			return nil, fmt.Errorf("invalid sysctl key %q in golden state", key)
		}
		want := golden.Sysctls[key]
		if !validateSysctlValue.MatchString(want) {
			return nil, fmt.Errorf("invalid sysctl value %q for %s in golden state", want, key)
		}
		path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
		lines = append(lines, "printf '%s' '"+want+"' > "+path)
	}
	return lines, nil
}

// validateGoldenState rejects any golden entry whose fields are not safe to
// interpolate into the reset shell. A kernel-captured golden state should
// never contain such values; if it does, the slot is quarantined instead of
// restored.
func validateGoldenState(golden *GoldenState) error {
	validators := []func(*GoldenState) error{
		validateGoldenLinks,
		validateGoldenAddresses,
		validateGoldenRoutes,
		validateGoldenNeighbours,
		validateGoldenQdiscs,
		validateGoldenSysctls,
	}
	for _, validate := range validators {
		if err := validate(golden); err != nil {
			return err
		}
	}
	return nil
}

func validateGoldenLinks(golden *GoldenState) error {
	for _, link := range golden.Links {
		if !validateDeviceName.MatchString(link.Name) {
			return fmt.Errorf("invalid device name %q in golden state", link.Name)
		}
		if link.MAC != "" && !validateMAC.MatchString(link.MAC) {
			return fmt.Errorf("invalid MAC %q for link %s in golden state", link.MAC, link.Name)
		}
	}
	return nil
}

func validateGoldenAddresses(golden *GoldenState) error {
	for _, address := range golden.Addresses {
		if !validateDeviceName.MatchString(address.Link) {
			return fmt.Errorf("invalid device name %q in golden address", address.Link)
		}
		if _, err := parseCIDROrError(address.Address); err != nil {
			return err
		}
	}
	return nil
}

func validateGoldenRoutes(golden *GoldenState) error {
	for _, route := range golden.Routes {
		if _, err := routeCommand("replace", route, true); err != nil {
			return err
		}
	}
	return nil
}

func validateGoldenNeighbours(golden *GoldenState) error {
	for _, neighbour := range golden.Neighbours {
		if !neighbour.Permanent {
			continue
		}
		if !validateDeviceName.MatchString(neighbour.Device) {
			return fmt.Errorf("invalid device name %q in golden neighbour", neighbour.Device)
		}
		if !validateMAC.MatchString(neighbour.MAC) {
			return fmt.Errorf("invalid neighbour MAC %q in golden state", neighbour.MAC)
		}
		if !validateRouteField.MatchString(neighbour.Destination) {
			return fmt.Errorf("invalid neighbour destination %q in golden state", neighbour.Destination)
		}
	}
	return nil
}

func validateGoldenQdiscs(golden *GoldenState) error {
	for _, qdisc := range golden.Qdiscs {
		if !validateDeviceName.MatchString(qdisc.Device) {
			return fmt.Errorf("invalid device name %q in golden qdisc", qdisc.Device)
		}
		if !validateRouteField.MatchString(qdisc.Parent) {
			return fmt.Errorf("invalid qdisc parent %q in golden state", qdisc.Parent)
		}
	}
	return nil
}

func validateGoldenSysctls(golden *GoldenState) error {
	for _, key := range sortedKeys(golden.Sysctls) {
		if !validateSysctlKey.MatchString(key) {
			return fmt.Errorf("invalid sysctl key %q in golden state", key)
		}
		if !validateSysctlValue.MatchString(golden.Sysctls[key]) {
			return fmt.Errorf("invalid sysctl value %q for %s in golden state", golden.Sysctls[key], key)
		}
	}
	return nil
}

func routeCommand(verb string, route GoldenRoute, restore bool) (string, error) {
	for _, field := range []string{route.Type, route.Destination, route.Gateway, route.Device, route.Table, route.Protocol, route.Scope} {
		if !validateRouteField.MatchString(field) {
			return "", fmt.Errorf("invalid route field %q in state", field)
		}
	}
	parts := []string{"ip route", verb}
	if route.Type != "" && route.Type != "unicast" {
		parts = append(parts, route.Type)
	}
	parts = append(parts, route.Destination)
	if route.Gateway != "" {
		parts = append(parts, "via", route.Gateway)
	}
	if route.Device != "" {
		parts = append(parts, "dev", route.Device)
	}
	if route.Table != "" && route.Table != "main" {
		parts = append(parts, "table", route.Table)
	}
	if route.Metric != 0 {
		parts = append(parts, "metric", strconv.Itoa(route.Metric))
	}
	if restore && route.Protocol != "" && route.Protocol != "boot" {
		parts = append(parts, "proto", route.Protocol)
	}
	if restore && route.Scope != "" {
		parts = append(parts, "scope", route.Scope)
	}
	return strings.Join(parts, " "), nil
}

func parseCIDROrError(cidr string) (string, error) {
	if !validateRouteField.MatchString(cidr) || !strings.Contains(cidr, "/") {
		return "", fmt.Errorf("invalid address %q in state", cidr)
	}
	return cidr, nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func addressInState(state []GoldenAddress, address GoldenAddress) bool {
	for _, candidate := range state {
		if addressKey(candidate) == addressKey(address) {
			return true
		}
	}
	return false
}

func routeInState(state []GoldenRoute, route GoldenRoute) bool {
	for _, candidate := range state {
		if routeKey(candidate) == routeKey(route) {
			return true
		}
	}
	return false
}

func qdiscInGolden(golden []GoldenQdisc, qdisc GoldenQdisc) bool {
	for _, candidate := range golden {
		if qdiscKey(candidate) == qdiscKey(qdisc) {
			return true
		}
	}
	return false
}
