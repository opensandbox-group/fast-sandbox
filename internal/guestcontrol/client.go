package guestcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Client connects through the private Firecracker host Unix socket.
// The runtime must bind the socket to the current VM incarnation and fence lifecycle operations.
type Client struct {
	SocketPath string
	Port       uint32
	Timeout    time.Duration
}

// Call opens a separate connection for each request and never automatically retries mutations.
// An error after sending Freeze leaves the outcome uncertain; recovery must use the same operation ID.
func (c Client) Call(ctx context.Context, r Request) (response Response, callErr error) {
	if c.SocketPath == "" || c.Port == 0 || c.Port == ^uint32(0) || c.Timeout <= 0 {
		return Response{}, errors.New("socket, port and positive timeout required")
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	defer func() {
		if callErr != nil && ctx.Err() != nil {
			callErr = errors.Join(callErr, ctx.Err())
		}
	}()
	conn, err := dialControlSocket(ctx, c.SocketPath)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return Response{}, err
	}
	if _, err = fmt.Fprintf(conn, "CONNECT %d\n", c.Port); err != nil {
		return Response{}, err
	}
	reader := bufio.NewReaderSize(conn, MaxRequestBytes+1)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return Response{}, err
	}
	fields := strings.Fields(string(line))
	if len(fields) != 2 || fields[0] != "OK" {
		return Response{}, errors.New("invalid Firecracker vsock acknowledgement")
	}
	if _, err = strconv.ParseUint(fields[1], 10, 32); err != nil {
		return Response{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return Response{}, err
	}
	if len(raw) > MaxRequestBytes {
		return Response{}, errors.New("request too large")
	}
	if _, err = conn.Write(append(raw, '\n')); err != nil {
		return Response{}, err
	}
	line, err = reader.ReadSlice('\n')
	if err != nil {
		return Response{}, err
	}
	if len(line) > MaxRequestBytes {
		return Response{}, errors.New("response too large")
	}
	var out Response
	if err = json.Unmarshal(line, &out); err != nil {
		return Response{}, err
	}
	if out.Protocol != Protocol || out.Epoch == "" {
		return Response{}, errors.New("invalid guest protocol response")
	}
	if r.Method != methodCapabilities && out.Epoch != r.Epoch {
		return out, errors.New("agent epoch changed")
	}
	if out.Error != "" {
		return out, fmt.Errorf("guest control: %s", out.Error)
	}
	if r.Method != methodCapabilities && out.Generation != r.Generation {
		return out, errors.New("response generation mismatch")
	}
	if r.Method != methodCapabilities {
		switch out.State {
		case Pending, Held, Released, Expired:
		default:
			return out, errors.New("invalid operation state")
		}
	}
	return out, nil
}

// FreezeAndWait sends freeze once, then polls until the ioctl completes.
// Any error leaves the outcome uncertain: callers must invoke ThawAndWait with the same identity and a fresh, bounded recovery context.
// The VM must remain running throughout this operation.
func (c Client) FreezeAndWait(ctx context.Context, request Request) (Response, error) {
	request.Method = methodFreeze
	response, err := c.Call(ctx, request)
	if err != nil {
		return response, err
	}
	return c.waitOperation(ctx, request, response, true)
}

// ThawAndWait records terminal intent even if freeze has not arrived yet.
// Call only after the VM resumes and capture no longer depends on filesystem protection.
// On error, quarantine the source instance and reconcile its state.
func (c Client) ThawAndWait(ctx context.Context, request Request) (Response, error) {
	request.Method = methodThaw
	request.LeaseMillis = 0
	response, err := c.Call(ctx, request)
	if err != nil {
		return response, err
	}
	return c.waitOperation(ctx, request, response, false)
}

func (c Client) waitOperation(ctx context.Context, request Request, response Response, acquiring bool) (Response, error) {
	request.Method = methodStatus
	request.LeaseMillis = 0
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if response.Failure != "" {
			return response, fmt.Errorf("guest filesystem operation failed: %s", response.Failure)
		}
		if acquiring {
			if response.State == Held && !response.CleanupPending {
				return response, nil
			}
			if response.State != Pending {
				return response, errors.New("filesystem protection terminated before acquisition")
			}
		} else if (response.State == Released || response.State == Expired) && !response.CleanupPending {
			return response, nil
		}
		select {
		case <-ctx.Done():
			return response, ctx.Err()
		case <-ticker.C:
		}
		var err error
		response, err = c.Call(ctx, request)
		if err != nil {
			return response, err
		}
	}
}

// Keep the directory descriptor alive until connect completes. The short alias
// preserves /proc/<pid>/root and cwd resolution without exceeding sun_path.
func dialControlSocket(ctx context.Context, path string) (net.Conn, error) {
	if runtime.GOOS != "linux" || len(path) < 108 {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	alias := fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), filepath.Base(path))
	return (&net.Dialer{}).DialContext(ctx, "unix", alias)
}
