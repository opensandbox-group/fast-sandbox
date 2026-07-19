package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, command string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", command, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type LinuxDriverConfig struct {
	Runner          CommandRunner
	ResolverPath    string
	IPCommand       string
	IPTablesCommand string
	SysctlCommand   string
}

type LinuxNetNSDriver struct {
	runner          CommandRunner
	resolverPath    string
	ipCommand       string
	iptablesCommand string
	sysctlCommand   string
}

func NewLinuxNetNSDriver(config LinuxDriverConfig) *LinuxNetNSDriver {
	if config.Runner == nil {
		config.Runner = ExecRunner{}
	}
	if config.ResolverPath == "" {
		config.ResolverPath = "/etc/resolv.conf"
	}
	if config.IPCommand == "" {
		config.IPCommand = "ip"
	}
	if config.IPTablesCommand == "" {
		config.IPTablesCommand = "iptables"
	}
	if config.SysctlCommand == "" {
		config.SysctlCommand = "sysctl"
	}
	return &LinuxNetNSDriver{
		runner: config.Runner, resolverPath: config.ResolverPath,
		ipCommand: config.IPCommand, iptablesCommand: config.IPTablesCommand, sysctlCommand: config.SysctlCommand,
	}
}

func (d *LinuxNetNSDriver) Prepare(ctx context.Context, slot *Slot) (result error) {
	if err := validateLinuxSlot(slot); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			_ = d.Destroy(context.Background(), slot)
		}
	}()
	if err := os.MkdirAll(filepath.Dir(slot.NetNSPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(slot.DNSPath), 0o750); err != nil {
		return err
	}
	resolver, err := os.ReadFile(d.resolverPath)
	if err != nil {
		return fmt.Errorf("read resolver configuration: %w", err)
	}
	if err := os.WriteFile(slot.DNSPath, resolver, 0o644); err != nil {
		return fmt.Errorf("write slot resolver configuration: %w", err)
	}

	if err := d.ensureBridge(ctx, slot); err != nil {
		return err
	}
	if err := d.ensureEgress(ctx, slot); err != nil {
		return err
	}
	if _, err := d.runner.Run(ctx, d.ipCommand, "netns", "add", slot.NetNSName); err != nil {
		return err
	}
	if _, err := d.runner.Run(ctx, d.ipCommand, "link", "add", slot.HostVeth, "mtu", fmt.Sprint(slot.MTU), "type", "veth", "peer", "name", slot.PeerVeth); err != nil {
		return err
	}
	if _, err := d.runner.Run(ctx, d.ipCommand, "link", "set", slot.HostVeth, "master", slot.Bridge); err != nil {
		return err
	}
	if _, err := d.runner.Run(ctx, d.ipCommand, "link", "set", slot.HostVeth, "up"); err != nil {
		return err
	}
	if _, err := d.runner.Run(ctx, d.ipCommand, "link", "set", slot.PeerVeth, "netns", slot.NetNSName); err != nil {
		return err
	}
	commands := [][]string{
		{"-n", slot.NetNSName, "link", "set", "lo", "up"},
		{"-n", slot.NetNSName, "link", "set", slot.PeerVeth, "name", "eth0"},
		{"-n", slot.NetNSName, "link", "set", "eth0", "mtu", fmt.Sprint(slot.MTU)},
		{"-n", slot.NetNSName, "addr", "add", slot.Address, "dev", "eth0"},
		{"-n", slot.NetNSName, "link", "set", "eth0", "up"},
		{"-n", slot.NetNSName, "route", "add", "default", "via", slot.Gateway, "dev", "eth0"},
	}
	for _, args := range commands {
		if _, err := d.runner.Run(ctx, d.ipCommand, args...); err != nil {
			return err
		}
	}
	// A Sandbox cannot reach a sibling private IP. The gateway remains
	// reachable for egress, and Fastlet Proxy ingress is unaffected.
	if _, err := d.runner.Run(ctx, d.ipCommand, "netns", "exec", slot.NetNSName,
		d.iptablesCommand, "-A", "OUTPUT", "-d", slot.Gateway+"/32", "-j", "ACCEPT"); err != nil {
		return err
	}
	if _, err := d.runner.Run(ctx, d.ipCommand, "netns", "exec", slot.NetNSName,
		d.iptablesCommand, "-A", "OUTPUT", "-d", slot.PrivateCIDR, "-j", "REJECT"); err != nil {
		return err
	}
	return nil
}

func (d *LinuxNetNSDriver) Validate(ctx context.Context, slot *Slot) error {
	if err := validateLinuxSlot(slot); err != nil {
		return err
	}
	if _, err := os.Stat(slot.NetNSPath); err != nil {
		return fmt.Errorf("network namespace mount: %w", err)
	}
	if _, err := os.Stat(slot.DNSPath); err != nil {
		return fmt.Errorf("resolver state: %w", err)
	}
	if _, err := d.runner.Run(ctx, d.ipCommand, "link", "show", "dev", slot.HostVeth); err != nil {
		return err
	}
	if _, err := d.runner.Run(ctx, d.ipCommand, "-n", slot.NetNSName, "addr", "show", "dev", "eth0"); err != nil {
		return err
	}
	return nil
}

func (d *LinuxNetNSDriver) Destroy(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}
	var result error
	if slot.NetNSName != "" {
		if _, err := d.runner.Run(ctx, d.ipCommand, "netns", "delete", slot.NetNSName); err != nil && !isMissingNetworkResource(err) {
			result = errors.Join(result, err)
		}
	}
	if slot.HostVeth != "" {
		if _, err := d.runner.Run(ctx, d.ipCommand, "link", "delete", slot.HostVeth); err != nil && !isMissingNetworkResource(err) {
			result = errors.Join(result, err)
		}
	}
	if slot.DNSPath != "" {
		if err := os.Remove(slot.DNSPath); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (d *LinuxNetNSDriver) ensureBridge(ctx context.Context, slot *Slot) error {
	if _, err := d.runner.Run(ctx, d.ipCommand, "link", "show", "dev", slot.Bridge); err != nil {
		if _, addErr := d.runner.Run(ctx, d.ipCommand, "link", "add", slot.Bridge, "type", "bridge"); addErr != nil {
			return addErr
		}
		if _, addrErr := d.runner.Run(ctx, d.ipCommand, "addr", "add", gatewayPrefix(slot), "dev", slot.Bridge); addrErr != nil {
			return addrErr
		}
	}
	_, err := d.runner.Run(ctx, d.ipCommand, "link", "set", slot.Bridge, "up")
	return err
}

func (d *LinuxNetNSDriver) ensureEgress(ctx context.Context, slot *Slot) error {
	if _, err := d.runner.Run(ctx, d.sysctlCommand, "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	egress := slot.EgressDevice
	if egress == "" {
		output, err := d.runner.Run(ctx, d.ipCommand, "-4", "route", "show", "default")
		if err != nil {
			return err
		}
		egress = defaultRouteDevice(string(output))
		if egress == "" {
			return fmt.Errorf("default IPv4 route has no egress device")
		}
		slot.EgressDevice = egress
	}
	check := []string{"-t", "nat", "-C", "POSTROUTING", "-s", slot.PrivateCIDR, "!", "-d", slot.PrivateCIDR, "-o", egress, "-j", "MASQUERADE"}
	if _, err := d.runner.Run(ctx, d.iptablesCommand, check...); err == nil {
		return nil
	}
	add := append([]string(nil), check...)
	add[2] = "-A"
	_, err := d.runner.Run(ctx, d.iptablesCommand, add...)
	return err
}

func validateLinuxSlot(slot *Slot) error {
	if slot == nil || slot.NetNSName == "" || slot.NetNSPath == "" || slot.HostNetNSPath == "" ||
		slot.HostVeth == "" || slot.PeerVeth == "" || slot.Bridge == "" || slot.Address == "" ||
		slot.IP == "" || slot.Gateway == "" || slot.PrivateCIDR == "" || slot.DNSPath == "" || slot.MTU <= 0 {
		return fmt.Errorf("incomplete Linux network slot")
	}
	return nil
}

func gatewayPrefix(slot *Slot) string {
	parts := strings.SplitN(slot.Address, "/", 2)
	if len(parts) != 2 {
		return slot.Gateway
	}
	return slot.Gateway + "/" + parts[1]
}

func defaultRouteDevice(output string) string {
	fields := strings.Fields(output)
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == "dev" {
			return fields[index+1]
		}
	}
	return ""
}

func isMissingNetworkResource(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "cannot find device") || strings.Contains(message, "no such file") ||
		strings.Contains(message, "cannot open network namespace")
}
