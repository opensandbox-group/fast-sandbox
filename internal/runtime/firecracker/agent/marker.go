package agent

// Verified-cache marker: after a cache file's content is verified (a
// digest-checked download, a chunked pull whose every chunk verified, or a
// publish of the staged bytes), the agent records the verified digest in an
// xattr on the inode together with the file's size, mtime and the boot ID.
// cacheComplete trusts a file whose marker still matches instead of
// re-hashing multi-GiB artifacts on every lease.
//
// The marker lives on the inode, so it survives the driver's renames
// (checkpoint cache commit) and is shared by the jail hard links. ctime is
// deliberately not part of it: creating the jail hard links bumps ctime on
// every sandbox create. The boot ID forces one re-verification after a
// reboot, which covers torn writes from a power loss.
//
// Assumption: cache artifacts are immutable once committed (sandboxes get
// reflink copies; Firecracker maps memory MAP_PRIVATE). mtime has the
// kernel's coarse clock granularity, so a same-size in-place rewrite within
// a few milliseconds of marking would not be detected; anything that edits
// a cache file must remove the xattr (or the file) instead.

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// verifiedMarkerAttr is the xattr carrying the marker.
const verifiedMarkerAttr = "user.fsb.verified"

// verifiedMarker is the marker payload.
type verifiedMarker struct {
	Version int    `json:"v"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtimeNs"`
	BootID  string `json:"bootId"`
}

var (
	bootIDOnce  sync.Once
	bootIDValue string
)

// currentBootID returns the kernel boot ID ("" when unavailable, which
// disables marker trust).
func currentBootID() string {
	bootIDOnce.Do(func() {
		payload, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err == nil {
			bootIDValue = strings.TrimSpace(string(payload))
		}
	})
	return bootIDValue
}

// markVerified records that path holds content with the given sha256. It is
// best-effort: a filesystem without user xattrs simply keeps the full
// re-hash on the next check.
func markVerified(path, sum string) {
	boot := currentBootID()
	if boot == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	payload, err := json.Marshal(verifiedMarker{
		Version: 1, SHA256: sum, Size: info.Size(), MtimeNs: info.ModTime().UnixNano(), BootID: boot,
	})
	if err != nil {
		return
	}
	_ = setXattr(path, verifiedMarkerAttr, payload)
}

// markerMatches reports whether path carries a marker for sum that still
// describes the file (same size, same mtime, same boot).
func markerMatches(path string, info os.FileInfo, sum string) bool {
	boot := currentBootID()
	if boot == "" {
		return false
	}
	payload, err := getXattr(path, verifiedMarkerAttr)
	if err != nil || len(payload) == 0 {
		return false
	}
	var marker verifiedMarker
	if err := json.Unmarshal(payload, &marker); err != nil {
		return false
	}
	return marker.Version == 1 && marker.SHA256 == sum && marker.Size == info.Size() &&
		marker.MtimeNs == info.ModTime().UnixNano() && marker.BootID == boot
}

// MarkVerified is the exported marker write for callers that verified a
// file themselves (the publish path marks the staged files it uploaded).
func MarkVerified(path, sum string) { markVerified(path, sum) }
