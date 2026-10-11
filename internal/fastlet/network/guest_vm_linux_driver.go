package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"time"

	"fast-sandbox/internal/guestnetwork"
)

// arpWarmupDialTimeout bounds the warm-up datagram dial in ApplyGuest.
const arpWarmupDialTimeout = 300 * time.Millisecond

// guestVMDefaultTapName is the fixed tap name of the guest-VM (Firecracker)
// data plane. The tap lives inside the slot network namespace (the VMM
// enters it via the jailer --netns), so the name is unique per namespace
// and carries no Pod identity. Slot.GuestTap keeps the field with this
// value so the runtime driver can reference the tap in the restore
// network_overrides without coupling to the network driver.
const (
	guestVMDefaultTapName = "vmtap0"
	guestDNATChain        = "FSB_GUEST_DNAT"
	guestSNATChain        = "FSB_GUEST_SNAT"
	guestNATChains        = "*nat\n:" + guestDNATChain + " - [0:0]\n:" + guestSNATChain + " - [0:0]\n"
	guestNATPrepareRules  = guestNATChains +
		"-A PREROUTING -j " + guestDNATChain + "\n" +
		"-A POSTROUTING -j " + guestSNATChain + "\nCOMMIT\n"
	guestNATApplyRules = guestNATChains +
		"-A " + guestDNATChain + " -d %s/32 -j DNAT --to-destination %s\n" +
		"-A " + guestSNATChain + " -s %s/32 -j SNAT --to-source %s\nCOMMIT\n"
)

// GuestVMNetNSDriver extends LinuxNetNSDriver with the per-clone netns data
// plane for guest-VM runtimes (Firecracker). The VMM process enters the slot
// network namespace via the jailer; the tap (vmtap0, fixed name) is created
// inside the namespace and carries the baked guest address; ingress DNAT
// (slot IP -> guest IP), egress source NAT (guest IP -> slot IP), reply
// acceptance, and sibling isolation all live inside the namespace. Clones
// sharing the snapshot's baked guest MAC/IP therefore never collide on ARP
// and every instance is uniquely addressed by its slot IP. Container
// runtimes keep using LinuxNetNSDriver and never see the tap or the rules.
type GuestVMNetNSDriver struct {
	LinuxNetNSDriver
	iptablesRestoreCommand string
	restoreRunner          inputCommandRunner
}

// NewGuestVMNetNSDriver wraps a LinuxNetNSDriver with the guest-VM additions.
func NewGuestVMNetNSDriver(config LinuxDriverConfig) *GuestVMNetNSDriver {
	driver := &GuestVMNetNSDriver{LinuxNetNSDriver: *NewLinuxNetNSDriver(config)}
	driver.iptablesRestoreCommand = config.IPTablesRestoreCommand
	if driver.iptablesRestoreCommand == "" {
		driver.iptablesRestoreCommand = driver.iptablesCommand + "-restore"
	}
	driver.restoreRunner, _ = driver.runner.(inputCommandRunner)
	switch driver.runner.(type) {
	case ExecRunner, *ExecRunner:
		if _, err := exec.LookPath(driver.iptablesRestoreCommand); err != nil {
			driver.restoreRunner = nil
		}
	}
	return driver
}

// Prepare runs the standard Linux preparation, then builds the static
// in-namespace data plane: the guest tap (fixed name), the forward rules,
// proxy ARP and netns ip_forward. The guest-specific NAT rules (DNAT/SNAT
// and the delivery route) are NOT installed here: slots are prepared before
// the image (and its baked guest address) is known, so they are applied per
// restore by ApplyGuest.
func (d *GuestVMNetNSDriver) Prepare(ctx context.Context, slot *Slot) error {
	if err := d.LinuxNetNSDriver.Prepare(ctx, slot); err != nil {
		return err
	}
	if slot.GuestTap == "" {
		slot.GuestTap = guestVMDefaultTapName
	}
	nsTap := func(arguments ...string) []string {
		return append([]string{ipObjectNetns, execSubcommand, slot.NetNSName, d.ipCommand}, arguments...)
	}
	nsIPTables := func(arguments ...string) []string {
		return append([]string{ipObjectNetns, execSubcommand, slot.NetNSName, d.iptablesCommand}, arguments...)
	}
	commands := [][]string{
		nsTap("tuntap", "add", netDevFlag, guestVMDefaultTapName, "mode", "tap"),
		nsTap(linkSubcommand, ipSetSubcommand, guestVMDefaultTapName, "address", guestnetwork.GatewayMAC),
		nsTap(linkSubcommand, ipSetSubcommand, guestVMDefaultTapName, "mtu", strconv.Itoa(slot.MTU)),
		nsTap(linkSubcommand, ipSetSubcommand, guestVMDefaultTapName, "up"),
		// Proxy ARP on the TAP only, so the guest can resolve its baked
		// gateway (the host bridge address, absent on the tap segment).
		// The all-interfaces knob MUST NOT be set: it would make eth0
		// proxy-answer the whole private CIDR, so every slot netns answers
		// the host's ARP for every slot IP with its own veth MAC — the host
		// neighbour cache then points slot IPs at random netns and packets
		// never reach the right one.
		{ipObjectNetns, execSubcommand, slot.NetNSName, d.sysctlCommand, "-w", "net.ipv4.conf." + guestVMDefaultTapName + ".proxy_arp=1"},
		// The kernel queues proxy ARP replies for proxy_delay (default 80
		// ticks = 800ms) so a real owner could answer first; on this tap the
		// proxy IS the only answer for the baked gateway, and the guest's
		// first egress packet (SYN-ACK!) blocks on it — the entire
		// first-request stall after restore (measured 792ms on vmtap0).
		{ipObjectNetns, execSubcommand, slot.NetNSName, d.sysctlCommand, "-w", "net.ipv4.neigh." + guestVMDefaultTapName + ".proxy_delay=0"},
		{ipObjectNetns, execSubcommand, slot.NetNSName, d.sysctlCommand, "-w", "net.ipv4.ip_forward=1"},
	}
	for _, arguments := range commands {
		if _, err := d.runner.Run(ctx, d.ipCommand, arguments...); err != nil {
			return fmt.Errorf("prepare guest-VM namespace tap: %w", err)
		}
	}
	// Faster ARP re-resolution inside the namespace (guest gateway via the
	// proxy-ARP tap, host gateway via eth0); the default 1 s retransmit
	// stalls the first guest packets after every restore. Best-effort: the
	// tuning is a latency optimization and must never fail preparation.
	for _, device := range []string{guestVMDefaultTapName, eth0Name} {
		_, _ = d.runner.Run(ctx, d.ipCommand, ipObjectNetns, execSubcommand, slot.NetNSName,
			d.sysctlCommand, "-w", "net.ipv4.neigh."+device+".retrans_time_ms=100")
	}
	rules := [][]string{
		// The namespace gateway (bridge address) stays reachable (DNS proxy
		// and gateway traffic); siblings are isolated before any generic
		// forward accept.
		nsIPTables("-A", "FORWARD", "-d", slot.Gateway+"/32", "-j", "ACCEPT"),
		// Sibling isolation: only traffic ORIGINATING from the guest and
		// destined to the private CIDR is rejected. Ingress and reply
		// packets arrive from eth0 (out vmtap0) and are not affected.
		nsIPTables("-A", "FORWARD", "-i", guestVMDefaultTapName, "-o", eth0Name, "-d", slot.PrivateCIDR, "-j", "REJECT"),
		// Guest egress through the namespace veth.
		nsIPTables("-A", "FORWARD", "-i", guestVMDefaultTapName, "-o", eth0Name, "-j", "ACCEPT"),
		// Ingress and replies (DNATed or direct guest address).
		nsIPTables("-A", "FORWARD", "-i", eth0Name, "-o", guestVMDefaultTapName, "-j", "ACCEPT"),
		// Established connections (reply path).
		nsIPTables("-A", "FORWARD", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"),
	}
	for _, arguments := range rules {
		if err := checkThenAdd(ctx, d.runner, d.ipCommand, arguments); err != nil {
			return fmt.Errorf("prepare guest-VM namespace rules: %w", err)
		}
	}
	if d.restoreRunner != nil {
		if err := d.restoreGuestNAT(ctx, slot, guestNATPrepareRules); err != nil {
			return fmt.Errorf("prepare guest NAT chains: %w", err)
		}
		slot.GuestNATBatch = true
	}
	return nil
}

// ApplyGuest installs the per-restore guest data plane for the baked guest
// address (manifest guestNetwork): the /32 delivery route via the tap and
// the ingress DNAT (slot IP -> guest IP) / egress source NAT (guest IP ->
// slot IP) rules. Every slot translates its slot IP to the SAME baked guest
// address (per-clone clone model); slot.IP+1 is NOT the guest address
// except for the first slot. New slots replace dedicated chains; old slots
// retain idempotent check-then-add.
func (d *GuestVMNetNSDriver) ApplyGuest(ctx context.Context, slot *Slot, guestIP string) error {
	if slot == nil || slot.NetNSName == "" || slot.IP == "" {
		return fmt.Errorf("incomplete guest-VM slot")
	}
	address, err := netip.ParseAddr(guestIP)
	if err != nil || !address.Is4() {
		return fmt.Errorf("invalid baked guest IP %q", guestIP)
	}
	slotAddress, err := netip.ParseAddr(slot.IP)
	if err != nil || !slotAddress.Is4() {
		return fmt.Errorf("invalid slot IP %q", slot.IP)
	}
	if slot.IP == guestIP {
		// The slot netns eth0 owns the slot IP; if it equals the guest
		// address the netns kernel would shadow the guest (answer for it
		// locally and refuse TCP). The guest-VM IPAM reserves the baked
		// address, so this indicates a misconfigured template or CIDR.
		return fmt.Errorf("baked guest IP %q collides with the slot IP", guestIP)
	}
	slot.GuestIP = guestIP
	// Ingress delivery: the baked guest address is routed via the tap. The
	// address is deliberately NOT assigned to the tap: a local address
	// would shadow the guest (the netns kernel would answer for the guest
	// IP itself and refuse TCP with no listener).
	routeCtx, finishRoute := startGuestApplyStage(ctx, "route_replace")
	_, err = d.runner.Run(routeCtx, d.ipCommand, "-n", slot.NetNSName, "route", "replace", guestIP+"/32", netDevFlag, guestVMDefaultTapName)
	finishRoute(err)
	if err != nil {
		return fmt.Errorf("apply guest delivery route: %w", err)
	}
	if slot.GuestNATBatch {
		natCtx, finishNAT := startGuestApplyStage(ctx, "nat_apply")
		payload := fmt.Sprintf(guestNATApplyRules, slot.IP, guestIP, guestIP, slot.IP)
		err := d.restoreGuestNAT(natCtx, slot, payload)
		finishNAT(err)
		if err != nil {
			return fmt.Errorf("apply guest NAT batch: %w", err)
		}
	} else if err := d.applyLegacyGuestNAT(ctx, slot, guestIP); err != nil {
		return err
	}
	// ARP warm-up: one datagram to the slot IP forces the host bridge to
	// resolve the address against the netns eth0 (which owns it) right
	// away, instead of letting the first business packet hit a stale or
	// incomplete neighbour entry. The netns answers ARP independently of
	// the VM state; the DNATed datagram itself is dropped until the guest
	// resumes, which is harmless. Best-effort.
	_, finishARP := startGuestApplyStage(ctx, "arp_warmup")
	conn, dialErr := net.DialTimeout("udp", net.JoinHostPort(slot.IP, "9"), arpWarmupDialTimeout)
	if dialErr == nil {
		_, dialErr = conn.Write([]byte{0})
		_ = conn.Close()
	}
	finishARP(dialErr)
	return nil
}

func (d *GuestVMNetNSDriver) restoreGuestNAT(ctx context.Context, slot *Slot, payload string) error {
	if d.restoreRunner == nil {
		return fmt.Errorf("guest NAT batch requires a stdin command runner")
	}
	_, err := d.restoreRunner.RunInput(ctx, []byte(payload), d.ipCommand,
		ipObjectNetns, execSubcommand, slot.NetNSName, d.iptablesRestoreCommand, "--noflush")
	return err
}

func (d *GuestVMNetNSDriver) applyLegacyGuestNAT(ctx context.Context, slot *Slot, guestIP string) error {
	nsIPTables := func(arguments ...string) []string {
		return append([]string{ipObjectNetns, execSubcommand, slot.NetNSName, d.iptablesCommand}, arguments...)
	}
	rules := [][]string{
		// Ingress: the uniquely addressed slot IP is DNATed to the baked
		// guest address before the namespace routing decision.
		nsIPTables("-t", "nat", "-A", "PREROUTING", "-d", slot.IP+"/32", "-j", "DNAT", "--to-destination", guestIP),
		// Egress source address: the shared baked guest IP becomes the
		// unique slot IP so upstream source-IP dispatch keeps working.
		nsIPTables("-t", "nat", "-A", "POSTROUTING", "-s", guestIP+"/32", "-j", "SNAT", "--to-source", slot.IP),
	}
	for index, arguments := range rules {
		stage := "dnat"
		if index == 1 {
			stage = "snat"
		}
		natCtx, finishNAT := startGuestApplyStage(ctx, stage)
		err := checkThenAdd(natCtx, d.runner, d.ipCommand, arguments)
		finishNAT(err)
		if err != nil {
			return fmt.Errorf("apply guest NAT rules: %w", err)
		}
	}
	return nil
}

// BakedGuestIP returns the baked guest address convention of the templates:
// the address directly after the first slot (gateway + 2). The builder and
// the E2E prep bake this static address into the snapshot; the authoritative
// per-template value is the manifest guestNetwork (applied via ApplyGuest).
func BakedGuestIP(slot *Slot) (string, error) {
	if slot == nil || slot.Gateway == "" {
		return "", fmt.Errorf("slot gateway is required")
	}
	gateway, err := netip.ParseAddr(slot.Gateway)
	if err != nil || !gateway.Is4() {
		return "", fmt.Errorf("invalid slot gateway %q", slot.Gateway)
	}
	guest := gateway.Next().Next()
	if !guest.IsValid() || !guest.Is4() || guest == gateway {
		return "", fmt.Errorf("no guest address after gateway %q", slot.Gateway)
	}
	return guest.String(), nil
}

// Validate checks the TAP identity before recovered slots become available.
// The manager rebuilds invalid Clean slots and rejects invalid Bound slots;
// validation must never change an active interface's MAC.
func (d *GuestVMNetNSDriver) Validate(ctx context.Context, slot *Slot) error {
	if err := d.LinuxNetNSDriver.Validate(ctx, slot); err != nil {
		return err
	}
	if slot.GuestTap == "" {
		return fmt.Errorf("guest-VM slot has no tap name")
	}
	output, err := d.runner.Run(ctx, d.ipCommand, "-j", "-n", slot.NetNSName, linkSubcommand, "show", netDevFlag, guestVMDefaultTapName)
	if err != nil {
		return fmt.Errorf("guest tap %s: %w", guestVMDefaultTapName, err)
	}
	var links []struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(output, &links); err != nil {
		return fmt.Errorf("decode guest tap %s: %w", guestVMDefaultTapName, err)
	}
	if len(links) != 1 || links[0].Address != guestnetwork.GatewayMAC {
		return fmt.Errorf("guest tap %s must use gateway MAC %s: %s", guestVMDefaultTapName, guestnetwork.GatewayMAC, output)
	}
	if slot.GuestNATBatch {
		for _, hook := range []struct{ chain, target string }{{"PREROUTING", guestDNATChain}, {"POSTROUTING", guestSNATChain}} {
			if _, err := d.runner.Run(ctx, d.ipCommand, ipObjectNetns, execSubcommand, slot.NetNSName,
				d.iptablesCommand, "-t", "nat", "-C", hook.chain, "-j", hook.target); err != nil {
				return fmt.Errorf("guest NAT hook %s: %w", hook.chain, err)
			}
		}
	}
	return nil
}

// Destroy removes the in-namespace rules (guest-specific NAT rules first
// when a baked guest address was applied, then the static forward rules),
// then runs the Linux destruction (which deletes the namespace; the tap
// vanishes with it).
func (d *GuestVMNetNSDriver) Destroy(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}
	var result error
	if slot.NetNSName != "" {
		nsIPTables := func(arguments ...string) []string {
			return append([]string{ipObjectNetns, execSubcommand, slot.NetNSName, d.iptablesCommand}, arguments...)
		}
		var rules [][]string
		if slot.GuestIP != "" && !slot.GuestNATBatch {
			rules = append(rules,
				nsIPTables("-t", "nat", "-D", "PREROUTING", "-d", slot.IP+"/32", "-j", "DNAT", "--to-destination", slot.GuestIP),
				nsIPTables("-t", "nat", "-D", "POSTROUTING", "-s", slot.GuestIP+"/32", "-j", "SNAT", "--to-source", slot.IP),
			)
		}
		rules = append(rules,
			nsIPTables("-D", "FORWARD", "-d", slot.Gateway+"/32", "-j", "ACCEPT"),
			nsIPTables("-D", "FORWARD", "-i", guestVMDefaultTapName, "-o", eth0Name, "-d", slot.PrivateCIDR, "-j", "REJECT"),
			nsIPTables("-D", "FORWARD", "-i", guestVMDefaultTapName, "-o", eth0Name, "-j", "ACCEPT"),
			nsIPTables("-D", "FORWARD", "-i", eth0Name, "-o", guestVMDefaultTapName, "-j", "ACCEPT"),
			nsIPTables("-D", "FORWARD", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"),
		)
		for _, arguments := range rules {
			// Best-effort: the rules disappear with the namespace, and
			// a missing namespace or rule must not fail the destroy.
			_, _ = d.runner.Run(ctx, d.ipCommand, arguments...)
		}
	}
	if err := d.LinuxNetNSDriver.Destroy(ctx, slot); err != nil {
		result = errorsJoin(result, err)
	}
	return result
}

// checkThenAdd installs an iptables rule only when it is not present yet.
func checkThenAdd(ctx context.Context, runner CommandRunner, command string, add []string) error {
	check := append([]string(nil), add...)
	for index, argument := range check {
		if argument == "-A" {
			check[index] = "-C"
			break
		}
	}
	if _, err := runner.Run(ctx, command, check...); err == nil {
		return nil
	}
	_, err := runner.Run(ctx, command, add...)
	return err
}

// GuestVMIP returns the guest address directly after the slot IP.
func GuestVMIP(slot *Slot) (string, error) {
	if slot == nil || slot.IP == "" || slot.PrivateCIDR == "" {
		return "", fmt.Errorf("slot IP and private CIDR are required")
	}
	address, err := netip.ParseAddr(slot.IP)
	if err != nil || !address.Is4() {
		return "", fmt.Errorf("invalid slot IP %q", slot.IP)
	}
	prefix, err := netip.ParsePrefix(slot.PrivateCIDR)
	if err != nil || !prefix.Contains(address) {
		return "", fmt.Errorf("slot IP %q is outside private CIDR %q", slot.IP, slot.PrivateCIDR)
	}
	guest := address.Next()
	if !prefix.Contains(guest) {
		return "", fmt.Errorf("no guest address after %q in %q", slot.IP, slot.PrivateCIDR)
	}
	return guest.String(), nil
}

func errorsJoin(left, right error) error {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return fmt.Errorf("%w; %w", left, right)
}
