package network

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

// Residue hygiene for pooled network slots.
//
// Destroy-per-slot makes cross-tenant leakage structurally impossible but
// pays the full netns create/destroy cost on every teardown. Return-style
// pooling removes that cost class, but a slot handed back to the clean pool
// carries whatever the previous tenant left inside its network namespace.
// Functional smoke tests do not see most of it: measured across injected
// residue classes, only link-down, missing-address, and shaping-qdisc
// residue break the next tenant outright — extra addresses, stale routes,
// stray permanent neighbours, MAC drift, and netns sysctl drift all leave a
// fully working tenant over silently carried state.
//
// The hygiene layer here defines "clean enough to hand to another tenant"
// operationally: a returned slot is clean iff a structural audit of its
// namespace state against a golden state captured at pool-build time is
// empty. The golden state is never assumed from defaults — fresh-netns
// state is counterintuitive (the loopback interface may carry no 127/8
// route before the first address add, a tap is admin-up but NO-CARRIER
// until a VMM attaches, qdisc defaults differ per link kind, and net.*
// sysctls are inherited from the creating namespace), so it is captured
// from the kernel after Prepare and verified, never synthesized.
//
// Domains audited, per the pooling proposal: link admin state, MAC, MTU,
// addresses, routes, neighbour entries, qdiscs, netns sysctls, firewall
// rules (they live inside the slot namespace), and foreign members
// (resident processes and listening sockets). Kernel-internal state such as
// the conntrack table is invisible to a userspace audit and is handled by an
// explicit flush during the deep reset.
//
// Reset is deliberately "always reset, then audit as verification": one
// audit pass (~12 ms) costs more than an unconditional reset, so gating the
// reset on an audit is strictly worse while remaining weaker, and reset
// failures are contained by quarantining the slot (destroy + rebuild).

// GoldenState is the audited, userspace-visible state of a slot network
// namespace at pool-build time. The zero value is not a valid golden state;
// it is always produced by CaptureGolden.
type GoldenState struct {
	Links      []GoldenLink      `json:"links,omitempty"`
	Addresses  []GoldenAddress   `json:"addresses,omitempty"`
	Routes     []GoldenRoute     `json:"routes,omitempty"`
	Neighbours []GoldenNeighbour `json:"neighbours,omitempty"`
	Qdiscs     []GoldenQdisc     `json:"qdiscs,omitempty"`
	Sysctls    map[string]string `json:"sysctls,omitempty"`
	Firewall   []string          `json:"firewall,omitempty"`
}

// GoldenLink is one link's identity and admin state. Operstate is
// deliberately not compared: a tap is NO-CARRIER until a VMM attaches, so
// admin-up is the golden criterion.
type GoldenLink struct {
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"`
	AdminUp bool   `json:"adminUp"`
	MTU     int    `json:"mtu,omitempty"`
	MAC     string `json:"mac,omitempty"`
}

// GoldenAddress is one address binding.
type GoldenAddress struct {
	Link    string `json:"link"`
	Family  string `json:"family"`
	Address string `json:"address"`
}

// GoldenRoute is one route table entry with the fields that define its
// identity for reuse purposes.
type GoldenRoute struct {
	Type        string `json:"type,omitempty"`
	Table       string `json:"table,omitempty"`
	Destination string `json:"destination"`
	Gateway     string `json:"gateway,omitempty"`
	Device      string `json:"device,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Source      string `json:"source,omitempty"`
	Metric      int    `json:"metric,omitempty"`
}

// GoldenNeighbour is one neighbour entry. Only permanent entries are part
// of the pool contract: they can be reinstated exactly, while dynamic
// entries are flushed on return and re-learned by the next tenant.
type GoldenNeighbour struct {
	Destination string `json:"destination"`
	Device      string `json:"device,omitempty"`
	MAC         string `json:"mac,omitempty"`
	Permanent   bool   `json:"permanent,omitempty"`
}

// GoldenQdisc is one qdisc. Options are not compared: a qdisc of a different
// kind is residue, and the kernel reinstates its per-link default once the
// residue qdisc is removed.
type GoldenQdisc struct {
	Device string `json:"device"`
	Kind   string `json:"kind"`
	Root   bool   `json:"root,omitempty"`
	Parent string `json:"parent,omitempty"`
}

// Audit domains reported on a Violation.
const (
	domainLink      = "link"
	domainAddress   = "address"
	domainRoute     = "route"
	domainNeighbour = "neighbour"
	domainQdisc     = "qdisc"
	domainSysctl    = "sysctl"
	domainFirewall  = "firewall"
	domainSocket    = "socket"
	domainForeign   = "foreign"
)

// Violation is one structural difference between a slot namespace and its
// golden state. Any violation quarantines the slot on return.
type Violation struct {
	Domain string
	Detail string
}

func (v Violation) String() string { return v.Domain + ": " + v.Detail }

// SlotHygienist is the optional driver interface that makes pooled return
// safe: capture the golden pool-build state, reset a returned slot back to
// it, and audit the result. Drivers that do not implement it keep
// destroy-per-slot semantics even when return-style pooling is enabled.
type SlotHygienist interface {
	CaptureGolden(ctx context.Context, slot *Slot) (*GoldenState, error)
	DeepReset(ctx context.Context, slot *Slot, golden *GoldenState) error
	Audit(ctx context.Context, slot *Slot, golden *GoldenState) ([]Violation, error)
}

// netState is a parsed capture of the slot namespace.
type netState struct {
	Links      []GoldenLink
	Addresses  []GoldenAddress
	Routes     []GoldenRoute
	Neighbours []GoldenNeighbour
	Qdiscs     []GoldenQdisc
	Sysctls    map[string]string
	Firewall   []string
	Listeners  []string
}

// captureSection is a marker written by the capture script; every command
// output section is delimited so the whole capture is one in-namespace
// execution instead of one exec per domain.
const captureSectionPrefix = "#FSB#"

// captureFailurePrefix marks a section whose command failed outright, as
// opposed to a section that is legitimately empty (a fresh namespace has no
// qdiscs on down links and no firewall tables).
const captureFailurePrefix = "#FSBERR#"

// netStateCaptureScript walks every audited userspace domain in one shell.
// Section commands after the first deliberately tolerate individual command
// failure so a partial capture still reaches the parser; missing sections
// surface as parse errors, and a command that is simply absent (ss, tc on a
// foreign node) degrades to an empty section instead of failing admission.
// iptables-save and sysctl are mandatory: their sections being empty is a
// capture error.
var netStateCaptureScript = "ip -j link show\n" +
	"echo '" + captureSectionPrefix + "ADDR'\n" +
	"ip -j addr show\n" +
	"echo '" + captureSectionPrefix + "ROUTE'\n" +
	"ip -j route show table all\n" +
	"echo '" + captureSectionPrefix + "NEIGH'\n" +
	"ip -j neigh show\n" +
	"echo '" + captureSectionPrefix + "QDISC'\n" +
	"tc -j qdisc show 2>/dev/null || echo '" + captureFailurePrefix + "QDISC'\n" +
	"echo '" + captureSectionPrefix + "SYSCTL'\n" +
	"sysctl -a --pattern '^net\\.' 2>/dev/null || echo '" + captureFailurePrefix + "SYSCTL'\n" +
	"echo '" + captureSectionPrefix + "FIREWALL'\n" +
	"iptables-save 2>/dev/null || echo '" + captureFailurePrefix + "FIREWALL'\n" +
	"echo '" + captureSectionPrefix + "LISTEN'\n" +
	"ss -H -t -u -l -n 2>/dev/null || echo '" + captureFailurePrefix + "LISTEN'\n"

func captureNetState(ctx context.Context, runner CommandRunner, nsName string) (*netState, error) {
	output, err := runner.Run(ctx, "ip", "netns", "exec", nsName, "/bin/sh", "-c", netStateCaptureScript)
	if err != nil {
		return nil, fmt.Errorf("capture network namespace state: %w", err)
	}
	return parseNetState(string(output))
}

func parseNetState(output string) (*netState, error) {
	sections := map[string][]string{}
	failures := map[string]bool{}
	current := "LINK"
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, captureFailurePrefix) {
			failures[strings.TrimPrefix(line, captureFailurePrefix)] = true
			continue
		}
		if strings.HasPrefix(line, captureSectionPrefix) {
			current = strings.TrimPrefix(line, captureSectionPrefix)
			if _, exists := sections[current]; !exists {
				sections[current] = []string{}
			}
			continue
		}
		if _, exists := sections[current]; !exists && current == "LINK" {
			sections[current] = []string{}
		}
		sections[current] = append(sections[current], line)
	}
	for _, domain := range []string{"QDISC", "SYSCTL", "FIREWALL"} {
		if failures[domain] {
			return nil, fmt.Errorf("capture network namespace state: %s command failed", strings.ToLower(domain))
		}
	}
	state := &netState{Sysctls: map[string]string{}}
	var err error
	if state.Links, err = parseLinks(sections["LINK"]); err != nil {
		return nil, err
	}
	if state.Addresses, err = parseAddresses(sections["ADDR"]); err != nil {
		return nil, err
	}
	if state.Routes, err = parseRoutes(sections["ROUTE"]); err != nil {
		return nil, err
	}
	if state.Neighbours, err = parseNeighbours(sections["NEIGH"]); err != nil {
		return nil, err
	}
	if state.Qdiscs, err = parseQdiscs(sections["QDISC"]); err != nil {
		return nil, err
	}
	if state.Sysctls, err = parseSysctls(sections["SYSCTL"]); err != nil {
		return nil, err
	}
	if state.Firewall, err = parseFirewall(sections["FIREWALL"]); err != nil {
		return nil, err
	}
	state.Listeners = parseListeners(sections["LISTEN"])
	if len(state.Links) == 0 {
		return nil, fmt.Errorf("capture network namespace state: no links reported")
	}
	return state, nil
}

type ipLinkJSON struct {
	IfName   string   `json:"ifname"`
	Flags    []string `json:"flags"`
	MTU      int      `json:"mtu"`
	Address  string   `json:"address"`
	LinkType string   `json:"link_type"`
}

func parseLinks(lines []string) ([]GoldenLink, error) {
	var raw []ipLinkJSON
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &raw); err != nil {
		return nil, fmt.Errorf("decode link state: %w", err)
	}
	links := make([]GoldenLink, 0, len(raw))
	for _, link := range raw {
		if link.IfName == "" {
			continue
		}
		links = append(links, GoldenLink{
			Name:    link.IfName,
			Type:    link.LinkType,
			AdminUp: containsString(link.Flags, "UP"),
			MTU:     link.MTU,
			MAC:     strings.ToLower(link.Address),
		})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Name < links[j].Name })
	return links, nil
}

type ipAddrJSON struct {
	IfName   string `json:"ifname"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

func parseAddresses(lines []string) ([]GoldenAddress, error) {
	var raw []ipAddrJSON
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &raw); err != nil {
		return nil, fmt.Errorf("decode address state: %w", err)
	}
	addresses := []GoldenAddress{}
	for _, link := range raw {
		for _, info := range link.AddrInfo {
			if info.Family != "inet" && info.Family != "inet6" {
				continue
			}
			addresses = append(addresses, GoldenAddress{
				Link:    link.IfName,
				Family:  info.Family,
				Address: formatPrefix(info.Local, info.PrefixLen),
			})
		}
	}
	sort.Slice(addresses, func(i, j int) bool { return addressKey(addresses[i]) < addressKey(addresses[j]) })
	return addresses, nil
}

type ipRouteJSON struct {
	Type     string `json:"type"`
	Dst      string `json:"dst"`
	Table    string `json:"table"`
	Protocol string `json:"protocol"`
	Gateway  string `json:"gateway"`
	Dev      string `json:"dev"`
	Metric   int    `json:"metric"`
	Scope    string `json:"scope"`
	PrefSrc  string `json:"prefsrc"`
}

func parseRoutes(lines []string) ([]GoldenRoute, error) {
	var raw []ipRouteJSON
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &raw); err != nil {
		return nil, fmt.Errorf("decode route state: %w", err)
	}
	routes := make([]GoldenRoute, 0, len(raw))
	for _, route := range raw {
		if route.Dst == "" {
			continue
		}
		routes = append(routes, GoldenRoute{
			Type:        route.Type,
			Table:       route.Table,
			Destination: route.Dst,
			Gateway:     route.Gateway,
			Device:      route.Dev,
			Protocol:    route.Protocol,
			Scope:       route.Scope,
			Source:      route.PrefSrc,
			Metric:      route.Metric,
		})
	}
	sort.Slice(routes, func(i, j int) bool { return routeKey(routes[i]) < routeKey(routes[j]) })
	return routes, nil
}

type ipNeighJSON struct {
	Dst    string   `json:"dst"`
	Dev    string   `json:"dev"`
	LLAddr string   `json:"lladdr"`
	State  []string `json:"state"`
}

func parseNeighbours(lines []string) ([]GoldenNeighbour, error) {
	var raw []ipNeighJSON
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &raw); err != nil {
		return nil, fmt.Errorf("decode neighbour state: %w", err)
	}
	neighbours := make([]GoldenNeighbour, 0, len(raw))
	for _, neighbour := range raw {
		if neighbour.Dst == "" {
			continue
		}
		neighbours = append(neighbours, GoldenNeighbour{
			Destination: neighbour.Dst,
			Device:      neighbour.Dev,
			MAC:         strings.ToLower(neighbour.LLAddr),
			Permanent:   containsString(neighbour.State, "PERMANENT"),
		})
	}
	sort.Slice(neighbours, func(i, j int) bool { return neighbourKey(neighbours[i]) < neighbourKey(neighbours[j]) })
	return neighbours, nil
}

type tcQdiscJSON struct {
	Dev    string `json:"dev"`
	Kind   string `json:"kind"`
	Root   bool   `json:"root"`
	Parent string `json:"parent"`
}

func parseQdiscs(lines []string) ([]GoldenQdisc, error) {
	trimmed := strings.TrimSpace(strings.Join(lines, "\n"))
	if trimmed == "" {
		return nil, fmt.Errorf("decode qdisc state: empty output")
	}
	var raw []tcQdiscJSON
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("decode qdisc state: %w", err)
	}
	qdiscs := make([]GoldenQdisc, 0, len(raw))
	for _, qdisc := range raw {
		if qdisc.Dev == "" || qdisc.Kind == "" {
			continue
		}
		qdiscs = append(qdiscs, GoldenQdisc{Device: qdisc.Dev, Kind: qdisc.Kind, Root: qdisc.Root, Parent: qdisc.Parent})
	}
	sort.Slice(qdiscs, func(i, j int) bool { return qdiscKey(qdiscs[i]) < qdiscKey(qdiscs[j]) })
	return qdiscs, nil
}

func parseSysctls(lines []string) (map[string]string, error) {
	sysctls := map[string]string{}
	for _, line := range lines {
		key, value, found := strings.Cut(line, " = ")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimRight(value, " \t")
		if !strings.HasPrefix(key, "net.") {
			continue
		}
		sysctls[key] = value
	}
	if len(sysctls) == 0 {
		return nil, fmt.Errorf("capture network namespace state: no net.* sysctls reported")
	}
	return sysctls, nil
}

var (
	iptablesChainLinePattern = regexp.MustCompile(`^:[A-Za-z0-9_-]+ [A-Za-z-]+ \[[0-9]+:[0-9]+\]$`)
	iptablesRuleLinePattern  = regexp.MustCompile(`^-A [A-Za-z0-9_-]+( .*)?$`)
	iptablesTablePattern     = regexp.MustCompile(`^\*[a-z0-9_]+$`)
)

// parseFirewall normalizes iptables-save output into the ordered rule lines.
// Generated-by comments and chain policy counters vary with wall clock and
// traffic and are stripped; rule order inside a chain is preserved because
// it is semantic.
func parseFirewall(lines []string) ([]string, error) {
	var normalized []string
	for _, line := range strings.Split(strings.Join(lines, "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case line == "COMMIT" || iptablesTablePattern.MatchString(line):
			normalized = append(normalized, line)
		case iptablesChainLinePattern.MatchString(line):
			normalized = append(normalized, line[:strings.Index(line, " [")])
		case iptablesRuleLinePattern.MatchString(line):
			normalized = append(normalized, line)
		default:
			return nil, fmt.Errorf("unrecognized iptables-save line %q", line)
		}
	}
	return normalized, nil
}

func parseListeners(lines []string) []string {
	var listeners []string
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		listeners = append(listeners, fields[0]+" "+fields[1])
	}
	return listeners
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func formatPrefix(local string, prefixLen int) string {
	return local + "/" + strconv.Itoa(prefixLen)
}

func addressKey(address GoldenAddress) string {
	return address.Link + "|" + address.Family + "|" + address.Address
}

func routeKey(route GoldenRoute) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%d",
		route.Type, route.Table, route.Destination, route.Gateway, route.Device,
		route.Protocol, route.Scope, route.Source, route.Metric)
}

func neighbourKey(neighbour GoldenNeighbour) string {
	return fmt.Sprintf("%s|%s|%s|%t", neighbour.Destination, neighbour.Device, neighbour.MAC, neighbour.Permanent)
}

func qdiscKey(qdisc GoldenQdisc) string {
	scope := qdisc.Parent
	if qdisc.Root {
		scope = "root"
	}
	return qdisc.Device + "|" + qdisc.Kind + "|" + scope
}

// diffGolden reports the structural differences between the audited state
// of a slot namespace and its golden state. The returned slice is empty iff
// the slot is clean enough to hand to another tenant.
func diffGolden(golden *GoldenState, current *netState) []Violation {
	violations := []Violation{}
	violations = append(violations, diffLinks(golden.Links, current.Links)...)
	violations = append(violations, diffAddresses(golden.Addresses, current.Addresses)...)
	violations = append(violations, diffRoutes(golden.Routes, current.Routes)...)
	violations = append(violations, diffNeighbours(golden.Neighbours, current.Neighbours)...)
	violations = append(violations, diffQdiscs(golden.Qdiscs, current.Qdiscs)...)
	violations = append(violations, diffSysctls(golden.Sysctls, current.Sysctls)...)
	violations = append(violations, diffFirewall(golden.Firewall, current.Firewall)...)
	for _, listener := range current.Listeners {
		violations = append(violations, Violation{Domain: domainSocket, Detail: "leftover listener " + listener})
	}
	return violations
}

func diffLinks(golden, current []GoldenLink) []Violation {
	currentByName := map[string]GoldenLink{}
	for _, link := range current {
		currentByName[link.Name] = link
	}
	violations := []Violation{}
	for _, want := range golden {
		got, exists := currentByName[want.Name]
		if !exists {
			violations = append(violations, Violation{Domain: domainLink, Detail: "missing link " + want.Name})
			continue
		}
		delete(currentByName, want.Name)
		if got.AdminUp != want.AdminUp {
			violations = append(violations, Violation{Domain: domainLink, Detail: fmt.Sprintf("link %s admin state %t, want %t", want.Name, got.AdminUp, want.AdminUp)})
		}
		if got.MAC != want.MAC {
			violations = append(violations, Violation{Domain: domainLink, Detail: fmt.Sprintf("link %s address %s, want %s", want.Name, got.MAC, want.MAC)})
		}
		if got.MTU != want.MTU {
			violations = append(violations, Violation{Domain: domainLink, Detail: fmt.Sprintf("link %s mtu %d, want %d", want.Name, got.MTU, want.MTU)})
		}
	}
	for name := range currentByName {
		violations = append(violations, Violation{Domain: domainLink, Detail: "unexpected link " + name})
	}
	return violations
}

func diffAddresses(golden, current []GoldenAddress) []Violation {
	want := map[string]struct{}{}
	for _, address := range golden {
		want[addressKey(address)] = struct{}{}
	}
	got := map[string]struct{}{}
	for _, address := range current {
		got[addressKey(address)] = struct{}{}
	}
	return setDiff("address", want, got)
}

func diffRoutes(golden, current []GoldenRoute) []Violation {
	want := map[string]struct{}{}
	for _, route := range golden {
		want[routeKey(route)] = struct{}{}
	}
	got := map[string]struct{}{}
	for _, route := range current {
		got[routeKey(route)] = struct{}{}
	}
	return setDiff("route", want, got)
}

func diffNeighbours(golden, current []GoldenNeighbour) []Violation {
	want := map[string]struct{}{}
	for _, neighbour := range golden {
		if neighbour.Permanent {
			want[neighbourKey(neighbour)] = struct{}{}
		}
	}
	got := map[string]struct{}{}
	for _, neighbour := range current {
		if neighbour.Permanent {
			got[neighbourKey(neighbour)] = struct{}{}
		}
	}
	return setDiff("neighbour", want, got)
}

func diffQdiscs(golden, current []GoldenQdisc) []Violation {
	if golden == nil && current == nil {
		return nil
	}
	want := map[string]struct{}{}
	for _, qdisc := range golden {
		want[qdiscKey(qdisc)] = struct{}{}
	}
	got := map[string]struct{}{}
	for _, qdisc := range current {
		got[qdiscKey(qdisc)] = struct{}{}
	}
	return setDiff("qdisc", want, got)
}

func diffSysctls(golden, current map[string]string) []Violation {
	violations := []Violation{}
	keys := make([]string, 0, len(golden))
	for key := range golden {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, volatile := volatileNetSysctls[key]; volatile {
			continue
		}
		got, exists := current[key]
		if !exists {
			violations = append(violations, Violation{Domain: domainSysctl, Detail: "missing " + key})
			continue
		}
		if got != golden[key] {
			violations = append(violations, Violation{Domain: domainSysctl, Detail: fmt.Sprintf("%s=%s, want %s", key, got, golden[key])})
		}
	}
	return violations
}

func diffFirewall(golden, current []string) []Violation {
	if len(golden) != len(current) {
		return []Violation{{Domain: "firewall", Detail: fmt.Sprintf("rule set has %d lines, want %d", len(current), len(golden))}}
	}
	for index := range golden {
		if golden[index] != current[index] {
			return []Violation{{Domain: "firewall", Detail: fmt.Sprintf("rule set differs at line %d: %q, want %q", index, current[index], golden[index])}}
		}
	}
	return nil
}

func setDiff(domain string, want, got map[string]struct{}) []Violation {
	missing := make([]string, 0)
	for key := range want {
		if _, exists := got[key]; !exists {
			missing = append(missing, key)
		}
	}
	extra := make([]string, 0)
	for key := range got {
		if _, exists := want[key]; !exists {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	violations := make([]Violation, 0, len(missing)+len(extra))
	for _, key := range missing {
		violations = append(violations, Violation{Domain: domain, Detail: "missing " + key})
	}
	for _, key := range extra {
		violations = append(violations, Violation{Domain: domain, Detail: "residue " + key})
	}
	return violations
}

// volatileNetSysctls are namespace-scoped counters that change as a side
// effect of traffic rather than tenant configuration; they are excluded
// from the sysctl audit and never restored. Everything else under net.* is
// tenant-mutable state, and a drifted value is residue.
var volatileNetSysctls = map[string]struct{}{
	"net.netfilter.nf_conntrack_count":        {},
	"net.netfilter.nf_conntrack_expect_count": {},
}

// procRoot is a seam for unit tests; process membership is always scanned
// from the host procfs in production.
var procRoot = "/proc"

// foreignMembers returns the PIDs whose network namespace is the one mounted
// at netnsPath. Any member is a leftover from the previous tenant: the
// workload process is gone by release time, and the capture subprocesses
// have already exited when this runs. /proc is scanned from the host so the
// check costs no additional exec.
func foreignMembers(procRoot, netnsPath string) ([]int, error) {
	target, err := os.Stat(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("stat slot network namespace: %w", err)
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", procRoot, err)
	}
	var members []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		info, err := os.Stat(filepath.Join(procRoot, entry.Name(), "ns", "net"))
		if err != nil {
			continue
		}
		if os.SameFile(target, info) {
			members = append(members, pid)
		}
	}
	sort.Ints(members)
	return members, nil
}

// CaptureGolden snapshots the audited state of a prepared slot. It is
// called after Prepare succeeds, so the snapshot is the kernel's own view
// of a fresh slot rather than an assumed default state.
func (d *LinuxNetNSDriver) CaptureGolden(ctx context.Context, slot *Slot) (*GoldenState, error) {
	if slot == nil || slot.NetNSName == "" {
		return nil, fmt.Errorf("incomplete Linux network slot")
	}
	state, err := captureNetState(ctx, d.runner, slot.NetNSName)
	if err != nil {
		return nil, err
	}
	return state.golden(), nil
}

// Audit compares the current namespace state with the golden state and
// reports every structural difference, including foreign members. An empty
// result is the pool contract for handing the slot to another tenant.
func (d *LinuxNetNSDriver) Audit(ctx context.Context, slot *Slot, golden *GoldenState) ([]Violation, error) {
	if slot == nil || slot.NetNSName == "" || golden == nil {
		return nil, fmt.Errorf("incomplete audit request")
	}
	current, err := captureNetState(ctx, d.runner, slot.NetNSName)
	if err != nil {
		return nil, err
	}
	violations := diffGolden(golden, current)
	members, err := foreignMembers(procRoot, slot.NetNSPath)
	if err != nil {
		return nil, err
	}
	for _, pid := range members {
		violations = append(violations, Violation{Domain: domainForeign, Detail: fmt.Sprintf("resident process pid %d", pid)})
	}
	return violations, nil
}

func (state *netState) golden() *GoldenState {
	golden := &GoldenState{
		Links:      state.Links,
		Addresses:  state.Addresses,
		Routes:     state.Routes,
		Neighbours: state.Neighbours,
		Qdiscs:     state.Qdiscs,
		Sysctls:    state.Sysctls,
		Firewall:   state.Firewall,
	}
	permanent := make([]GoldenNeighbour, 0, len(golden.Neighbours))
	for _, neighbour := range golden.Neighbours {
		if neighbour.Permanent {
			permanent = append(permanent, neighbour)
		}
	}
	golden.Neighbours = permanent
	return golden
}

// conntrackFlush is a seam for unit tests; the real implementation issues a
// single netlink request inside the slot namespace.
var conntrackFlush = flushConntrack

// LogAuditViolations emits audit findings at warning level; quarantine
// decisions are visible in logs and metrics, not only in a counter.
func logAuditViolations(slotID string, violations []Violation) {
	for _, violation := range violations {
		klog.Warningf("network slot %s residue audit: %s", slotID, violation)
	}
}
