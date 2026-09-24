package firecracker

// kvm_device.go implements the jailed KVM device alignment. The Firecracker
// jailer unconditionally creates <jail>/dev/kvm with the upstream default
// misc minor (10:232, firecracker src/jailer/src/env.rs DEV_KVM_MINOR) and
// fails with EEXIST when the inode pre-exists, so the real numbers cannot be
// provided before launch. Vendor KVM stacks register the misc device with a
// runtime-assigned minor (Alibaba kvm_c786a0c: 10:125) and may not expose
// the upstream sysfs path, so the jailed inode can point at a driver that
// does not exist: the jailed Firecracker then fails its first KVM object
// creation with a confusing ENODEV ("No such device (os error 19)") that is
// easy to misread as an ACL problem. The driver therefore rebinds the inode
// after the jailer launched and before the first restore API call — the KVM
// device is only opened when the restore builds the microVM, and replacing
// the inode of a running VMM is safe. The host /dev/kvm is never touched
// (the fix lives inside the jail directory), so host-level KVM consumers
// (e.g. Kata) sharing the device are unaffected.

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"k8s.io/klog/v2"
)

// hostKVMDevicePath is the container-visible path of the host KVM device
// (bind-mounted from the host into the Fastlet Pod), carrying the device
// numbers the node kernel actually registered.
const hostKVMDevicePath = "/dev/kvm"

// jailedKVMDevicePath returns the jail-internal path of the KVM device the
// jailer created.
func jailedKVMDevicePath(jailRoot string) string {
	return filepath.Join(jailRoot, "dev", "kvm")
}

// defaultAlignJailKVM is the production aligner: it runs the alignment
// logic with the platform syscall bindings (kvmStat, rebindCharDevice,
// defined per platform).
func defaultAlignJailKVM(hostPath, jailPath string) error {
	return alignJailKVMInode(kvmStat, rebindCharDevice, hostPath, jailPath)
}

// alignJailKVMInode compares the jailed KVM inode against the host device
// and rebinds it (remove + mknod with the host numbers) when the numbers
// differ. statFn and rebindFn are injectable so tests run without device
// nodes; the Linux build binds them to os.Stat and an mknod(2) call.
func alignJailKVMInode(
	statFn func(string) (os.FileInfo, error),
	rebindFn func(path string, rdev uint64) error,
	hostPath, jailPath string,
) error {
	hostInfo, err := statFn(hostPath)
	if err != nil {
		return fmt.Errorf("stat host KVM device %s: %w", hostPath, err)
	}
	hostRdev, ok := charDeviceRdev(hostInfo)
	if !ok {
		return fmt.Errorf("host KVM device %s is not a character device", hostPath)
	}
	jailInfo, err := statFn(jailPath)
	if err != nil {
		return fmt.Errorf("stat jailed KVM device %s (the jailer must have created it before the restore): %w", jailPath, err)
	}
	jailRdev, ok := charDeviceRdev(jailInfo)
	if !ok {
		return fmt.Errorf("jailed KVM device %s is not a character device", jailPath)
	}
	if jailRdev == hostRdev {
		return nil
	}
	klog.InfoS("rebinding the jailed KVM device to the host device numbers",
		"path", jailPath, "jailed", jailRdev, "host", hostRdev)
	return rebindFn(jailPath, hostRdev)
}

// charDeviceRdev returns the kernel device number of a character device
// FileInfo; it reports false for any other file kind or platform type.
func charDeviceRdev(info os.FileInfo) (uint64, bool) {
	if info.Mode()&os.ModeCharDevice == 0 {
		return 0, false
	}
	raw, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(raw.Rdev), true
}
