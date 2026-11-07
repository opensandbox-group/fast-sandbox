//go:build linux && (amd64 || arm64)

package guestcontrol

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ServeVSock accepts only the host CID. Access to the Firecracker host Unix socket must be restricted to the runtime service.
// This does not defend against Guest root. The listener shares the process lifetime; reconcile state before restarting service.
func (s *Service) ServeVSock(port uint32) error {
	if port == 0 || port == unix.VMADDR_PORT_ANY {
		return errors.New("explicit vsock port required")
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		return err
	}
	if err = unix.Listen(fd, 16); err != nil {
		return err
	}
	slots := make(chan struct{}, 16)
	for {
		conn, peer, err := unix.Accept4(fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		addr, ok := peer.(*unix.SockaddrVM)
		if !ok || addr.CID != unix.VMADDR_CID_HOST {
			unix.Close(conn)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			unix.Close(conn)
			continue
		}
		go func() {
			defer func() { <-slots }()
			file := os.NewFile(uintptr(conn), "guest-control-vsock")
			defer file.Close()
			if err := file.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			// Disconnecting does not cancel protection. The lease and explicit thaw handle lost responses,
			// including requests accepted before the caller times out.
			_ = s.ServeOne(file, file)
		}()
	}
}
