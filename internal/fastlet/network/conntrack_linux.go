//go:build linux

package network

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Netlink constants for the conntrack table flush. golang.org/x/sys/unix
// does not export the netfilter generic-netlink families, so the values are
// duplicated from the kernel headers (netfilter/nfnetlink.h,
// netfilter/nfnetlink_conntrack.h).
const (
	nfnetlinkSubsysCTNetlink = 1 // NFNL_SUBSYS_CTNETLINK
	ipctnlMsgCTDelete        = 2 // IPCTNL_MSG_CT_DELETE
	nfnetlinkVersion0        = 0 // NFNETLINK_V0
	nfnetlinkAFUnspec        = 0 // AF_UNSPEC: the whole table, both families

	conntrackFlushReplyTimeout = 500 * time.Millisecond
)

// nfgenmsg is the generic netlink message header (struct nfgenmsg).
type nfgenmsg struct {
	Family  uint8
	Version uint8
	ResID   uint16
}

// flushConntrack removes every conntrack entry owned by the network
// namespace mounted at netnsPath.
//
// The conntrack table is per-namespace and invisible to a userspace audit,
// so a returned slot cannot be declared clean from namespace state alone:
// established entries of the previous tenant (kept alive by the DNAT/SNAT
// rules of the guest data plane) must not survive into the next tenant.
// The flush is one netlink request, so it costs no fork/exec.
//
// The request must be issued from inside the namespace, which pins the
// calling goroutine to one OS thread for the duration (setns is per
// thread). The thread is always restored; if restoration fails, the thread
// is deliberately kept pinned and never returned to the runtime pool so no
// unrelated goroutine can be scheduled into the slot namespace.
func flushConntrack(netnsPath string) error {
	hostInfo, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("stat host network namespace: %w", err)
	}
	targetInfo, err := os.Stat(netnsPath)
	if err != nil {
		return fmt.Errorf("stat slot network namespace: %w", err)
	}
	if os.SameFile(hostInfo, targetInfo) {
		// Refuse to flush a namespace the Fastlet itself lives in: the
		// request would wipe the node's own conntrack table.
		return fmt.Errorf("refusing conntrack flush: slot namespace is the current namespace")
	}
	host, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("open host network namespace: %w", err)
	}
	defer host.Close()
	target, err := os.Open(netnsPath)
	if err != nil {
		return fmt.Errorf("open slot network namespace: %w", err)
	}
	defer target.Close()

	runtime.LockOSThread()
	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("enter slot network namespace: %w", err)
	}
	flushErr := sendConntrackFlush()
	if restoreErr := unix.Setns(int(host.Fd()), unix.CLONE_NEWNET); restoreErr != nil {
		// Keep the thread pinned: leaving it in the slot namespace would
		// run unrelated goroutines' syscalls against tenant state.
		runtime.LockOSThread()
		return fmt.Errorf("restore host network namespace: %w", errors.Join(restoreErr, flushErr))
	}
	runtime.UnlockOSThread()
	if flushErr != nil {
		return fmt.Errorf("send conntrack flush: %w", flushErr)
	}
	return nil
}

func sendConntrackFlush() error {
	socket, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_NETFILTER)
	if err != nil {
		return fmt.Errorf("open netlink socket: %w", err)
	}
	defer unix.Close(socket)
	address := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Bind(socket, address); err != nil {
		return fmt.Errorf("bind netlink socket: %w", err)
	}
	timeout := unix.NsecToTimeval(conntrackFlushReplyTimeout.Nanoseconds())
	_ = unix.SetsockoptTimeval(socket, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout)
	_ = unix.SetsockoptTimeval(socket, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout)

	// A CT_DELETE with an empty payload and AF_UNSPEC removes every entry in
	// the table of the issuing namespace (the same request conntrack -F
	// issues), and the ACK flag makes the kernel report failures.
	const headerLength = 16
	request := make([]byte, headerLength+unsafe.Sizeof(nfgenmsg{}))
	length := uint32(len(request))
	messageType := uint16(nfnetlinkSubsysCTNetlink<<8 | ipctnlMsgCTDelete)
	request[0], request[1], request[2], request[3] = byte(length), byte(length>>8), byte(length>>16), byte(length>>24)
	request[4], request[5] = byte(messageType), byte(messageType>>8)
	flags := uint16(unix.NLM_F_REQUEST | unix.NLM_F_ACK)
	request[6], request[7] = byte(flags), byte(flags>>8)
	// Sequence stays 1 and the port ID stays 0 (assigned by the kernel on
	// bind).
	request[8], request[9], request[10], request[11] = 1, 0, 0, 0
	message := (*nfgenmsg)(unsafe.Pointer(&request[headerLength]))
	message.Family = nfnetlinkAFUnspec
	message.Version = nfnetlinkVersion0
	message.ResID = 0

	if err := unix.Sendto(socket, request, 0, address); err != nil {
		return fmt.Errorf("send flush request: %w", err)
	}
	reply := make([]byte, 4096)
	for {
		read, _, err := unix.Recvfrom(socket, reply, 0)
		if err != nil {
			return fmt.Errorf("read flush reply: %w", err)
		}
		for offset := 0; offset+16 <= read; {
			messageLength := uint32(reply[offset]) | uint32(reply[offset+1])<<8 | uint32(reply[offset+2])<<16 | uint32(reply[offset+3])<<24
			if messageLength < 16 || offset+int(messageLength) > read {
				return fmt.Errorf("malformed flush reply")
			}
			replyType := uint16(reply[offset+4]) | uint16(reply[offset+5])<<8
			switch replyType {
			case unix.NLMSG_DONE:
				return nil
			case unix.NLMSG_ERROR:
				errno := int32(uint32(reply[offset+16]) | uint32(reply[offset+17])<<8 | uint32(reply[offset+18])<<16 | uint32(reply[offset+19])<<24)
				if errno != 0 {
					return fmt.Errorf("conntrack flush rejected: %w", unix.Errno(-errno))
				}
				return nil
			}
			offset += int(messageLength)
		}
	}
}
