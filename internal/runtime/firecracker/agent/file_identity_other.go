//go:build !linux && !darwin

package agent

import "os"

func fileIdentityFromInfo(_ os.FileInfo) (fileIdentity, bool) {
	return fileIdentity{}, false
}
