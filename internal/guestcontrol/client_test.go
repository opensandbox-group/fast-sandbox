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
	"strings"
	"testing"
	"time"
)

func TestClientUsesFirecrackerHandshake(t *testing.T) {
	// Use a short path to stay within the platform's Unix socket path limit.
	dir, err := os.MkdirTemp("", "fsbgc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "v.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m, _ := fixture(t)
	s, _ := NewService(m)
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		reader := bufio.NewReader(c)
		line, err := reader.ReadString('\n')
		if err != nil {
			done <- err
			return
		}
		if line != "CONNECT 1234\n" {
			done <- &net.AddrError{Err: "unexpected handshake", Addr: line}
			return
		}
		if _, err = c.Write([]byte("OK 999\n")); err != nil {
			done <- err
			return
		}
		done <- s.ServeOne(reader, c)
	}()
	out, err := (Client{SocketPath: path, Port: 1234, Timeout: time.Second}).Call(context.Background(), Request{Protocol: Protocol, Method: "capabilities"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Epoch != s.epoch {
		t.Fatal("wrong epoch")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func scriptedSocket(t *testing.T, script func(net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fsbgc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "v.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		script(c)
	}()
	return path
}
func TestClientRejectsBadReplies(t *testing.T) {
	for _, reply := range []string{
		`{"protocol":"fsb-freeze-v1","epoch":"old","generation":1,"state":"held"}`,
		`{"protocol":"fsb-freeze-v1","epoch":"current","generation":2,"state":"held"}`,
		`{"protocol":"fsb-freeze-v1","epoch":"current","generation":1,"state":"unknown"}`,
		`{"protocol":"fsb-freeze-v1","epoch":"current","generation":1,"error":"rejected"}`,
		strings.Repeat("x", MaxRequestBytes+20),
	} {
		t.Run(reply[:min(35, len(reply))], func(t *testing.T) {
			path := scriptedSocket(t, func(c net.Conn) {
				reader := bufio.NewReader(c)
				reader.ReadString('\n')
				c.Write([]byte("OK 999\n"))
				reader.ReadString('\n')
				c.Write([]byte(reply + "\n"))
			})
			_, err := (Client{SocketPath: path, Port: 1234, Timeout: time.Second}).Call(context.Background(), Request{Protocol: Protocol, Epoch: "current", Generation: 1, OperationID: "one", Method: "status"})
			if err == nil {
				t.Fatal("invalid reply accepted")
			}
		})
	}
}
func TestClientCancellationInterruptsHandshake(t *testing.T) {
	arrived := make(chan struct{})
	path := scriptedSocket(t, func(c net.Conn) {
		reader := bufio.NewReader(c)
		reader.ReadString('\n')
		close(arrived)
		var b [1]byte
		c.Read(b[:]) // wait for client cancellation to close the socket
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (Client{SocketPath: path, Port: 1234, Timeout: time.Second}).Call(ctx, Request{Protocol: Protocol, Method: "capabilities"})
		done <- err
	}()
	<-arrived
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock client")
	}
}

func operationClient(t *testing.T, service *Service) Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "fsbop-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "v.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close(); os.RemoveAll(dir) })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				r := bufio.NewReader(conn)
				r.ReadString('\n')
				conn.Write([]byte("OK 123\n"))
				service.ServeOne(r, conn)
			}()
		}
	}()
	return Client{SocketPath: path, Port: 1234, Timeout: time.Second}
}

func TestFreezeWaitCancellationThenLateCompletionCleanup(t *testing.T) {
	manager, fs := fixture(t)
	service, _ := NewService(manager)
	client := operationClient(t, service)
	request := Request{Protocol: Protocol, Epoch: service.epoch, Generation: 1, OperationID: "capture", LeaseMillis: 10000}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.FreezeAndWait(ctx, request); done <- err }()
	<-fs.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("freeze: %v", err)
	}
	recovery, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	go func() { _, err := client.ThawAndWait(recovery, request); done <- err }()
	waitStatus(t, manager, func(s Status) bool { return s.State == Released && s.CleanupPending })
	close(fs.complete)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fs.thawCount.Load() != 1 {
		t.Fatal("late freeze not compensated")
	}
	if _, err := client.FreezeAndWait(recovery, request); err == nil {
		t.Fatal("terminal operation reacquired")
	}
}

func TestFreezeWaitRejectsAsynchronousFailure(t *testing.T) {
	manager, fs := fixture(t)
	fs.freezeErr = errors.New("ioctl failed")
	close(fs.complete)
	service, _ := NewService(manager)
	client := operationClient(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.FreezeAndWait(ctx, Request{Protocol: Protocol, Epoch: service.epoch, Generation: 1, OperationID: "capture", LeaseMillis: 10000})
	if err == nil || response.Failure == "" {
		b, _ := json.Marshal(response)
		t.Fatalf("response %s, err %v", b, err)
	}
}

func TestClientDialsLongProcSocketPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux procfs")
	}
	for _, name := range []string{"guest-control.sock", "restored.sock"} {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), strings.Repeat("d", 110))
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			fd, err := os.Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			alias := fmt.Sprintf("/proc/self/fd/%d/%s", fd.Fd(), name)
			listener, err := net.Listen("unix", alias)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			// Connect through the same proc-root prefix used for direct and jailed VMMs.
			path := fmt.Sprintf("/proc/%d/root%s/%s", os.Getpid(), directory, name)
			conn, err := dialControlSocket(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			missing := filepath.Join(directory, "missing", name)
			if conn, err := dialControlSocket(t.Context(), missing); err == nil {
				conn.Close()
				t.Fatal("accepted missing parent")
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			if conn, err := dialControlSocket(canceled, path); err == nil {
				conn.Close()
				t.Fatal("ignored canceled context")
			}
		})
	}
}
