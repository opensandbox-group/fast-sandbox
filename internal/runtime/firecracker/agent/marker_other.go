//go:build !linux

package agent

import "errors"

// errNoXattr keeps the marker disabled off Linux: every cache check falls
// back to the full re-hash.
var errNoXattr = errors.New("xattrs are only used on linux")

func setXattr(string, string, []byte) error { return errNoXattr }

func getXattr(string, string) ([]byte, error) { return nil, errNoXattr }
