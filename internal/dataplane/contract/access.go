// Package contract defines the runtime-neutral data-plane protocol shared by
// Fastlet, runtime adapters, and proxy processes.
package contract

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

type AccessKind string

const (
	AccessKindDirectIP     AccessKind = "DirectIP"
	AccessKindLocalForward AccessKind = "LocalForward"
)

// AccessDescriptor is the durable, Fastlet-local dial description published
// to Fastlet Proxy. It is deliberately not part of the Sandbox CRD.
type AccessDescriptor struct {
	Kind       AccessKind `json:"kind"`
	Address    string     `json:"address"`
	NetNSPath  string     `json:"netnsPath,omitempty"`
	Credential string     `json:"credential,omitempty"`
}

func (a AccessDescriptor) Validate() error {
	switch a.Kind {
	case AccessKindDirectIP:
		if net.ParseIP(a.Address) == nil {
			return errors.New("DirectIP access descriptor requires an IP address")
		}
		if a.Credential != "" {
			return errors.New("DirectIP access descriptor cannot carry a LocalForward credential")
		}
	case AccessKindLocalForward:
		host, port, err := net.SplitHostPort(a.Address)
		if err != nil {
			return fmt.Errorf("LocalForward access descriptor requires loopback host:port: %w", err)
		}
		ip := net.ParseIP(host)
		parsedPort, portErr := strconv.ParseUint(port, 10, 16)
		if ip == nil || !ip.IsLoopback() || portErr != nil || parsedPort == 0 {
			return errors.New("LocalForward access descriptor requires loopback host:port")
		}
		if err := ValidateLocalForwardCredential(a.Credential); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported access kind %q", a.Kind)
	}
	return nil
}
