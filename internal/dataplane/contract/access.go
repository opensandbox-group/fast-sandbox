// Package contract defines the runtime-neutral data-plane protocol shared by
// Fastlet, runtime adapters, and proxy processes.
package contract

import (
	"errors"
	"fmt"
	"net"
)

type AccessKind string

const (
	AccessKindDirectIP AccessKind = "DirectIP"
)

// AccessDescriptor is the durable, Fastlet-local dial description published
// to Fastlet Proxy. It is deliberately not part of the Sandbox CRD.
type AccessDescriptor struct {
	Kind      AccessKind `json:"kind"`
	Address   string     `json:"address"`
	NetNSPath string     `json:"netnsPath,omitempty"`
}

func (a AccessDescriptor) Validate() error {
	switch a.Kind {
	case AccessKindDirectIP:
		if net.ParseIP(a.Address) == nil {
			return errors.New("DirectIP access descriptor requires an IP address")
		}
	default:
		return fmt.Errorf("unsupported access kind %q", a.Kind)
	}
	return nil
}
