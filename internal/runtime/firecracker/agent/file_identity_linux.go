//go:build linux

package agent

import (
	"os"
	"syscall"
)

func fileIdentityFromInfo(info os.FileInfo) (fileIdentity, bool) {
	if !info.Mode().IsRegular() {
		return fileIdentity{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{
		dev:       uint64(st.Dev),
		ino:       uint64(st.Ino),
		size:      info.Size(),
		mtimeNano: st.Mtim.Nano(),
		ctimeNano: st.Ctim.Nano(),
	}, true
}
