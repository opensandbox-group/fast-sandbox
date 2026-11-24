//go:build !linux

package network

func detachNetNSAliases(string) error { return nil }
