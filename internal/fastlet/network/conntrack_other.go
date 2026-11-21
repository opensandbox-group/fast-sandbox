//go:build !linux

package network

import "errors"

// errConntrackUnsupported reports that the conntrack table cannot be flushed
// on this node; pooled return must quarantine the slot instead, because
// kernel-internal conntrack entries are invisible to the audit.
var errConntrackUnsupported = errors.New("conntrack flush is not supported on this platform")

// flushConntrack is unavailable outside Linux.
func flushConntrack(string) error { return errConntrackUnsupported }
