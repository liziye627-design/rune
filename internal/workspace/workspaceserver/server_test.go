// Copyright (C) 2017-2026 The Rune Authors
// SPDX-License-Identifier: GPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at
// your option) any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
// General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package workspaceserver

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpc "google.golang.org/grpc"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace/workspacetest"
)

func TestSocketPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		path func(string) string
		base string
	}{
		{name: "rpc socket", path: SocketPath, base: "rune.sock"},
		{name: "attachment socket", path: VTESocketPath, base: "rune.vte.sock"},
		{name: "lock", path: LockPath, base: "rune.lock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tc.path("/data")
			assert.Equal(t, SocketDir("/data"), filepath.Dir(path),
				"everything a server exposes lives in the private socket dir")
			assert.Equal(t, tc.base, filepath.Base(path))
			assert.Equal(t, path, tc.path("/data"),
				"a server must be found at the same place across Rune restarts")
			assert.NotEqual(t, path, tc.path("/other"),
				"each data directory has its own server")
		})
	}
}

func TestListen(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// socket prepares dir and returns the path to listen on.
		socket  func(t *testing.T, dir string) string
		wantMsg string
		check   func(t *testing.T, socket string)
	}{
		{
			name: "keeps the socket private",
			socket: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "nested", "perm.sock")
			},
			check: func(t *testing.T, socket string) {
				info, err := os.Stat(socket)
				require.NoError(t, err)
				assert.Equal(t, socketPerm, info.Mode().Perm())
				dir, err := os.Stat(filepath.Dir(socket))
				require.NoError(t, err)
				assert.Equal(t, dirPerm, dir.Mode().Perm())
			},
		},
		{
			name: "reclaims a socket left by a dead server",
			socket: func(t *testing.T, dir string) string {
				socket := filepath.Join(dir, "stale.sock")
				// A server killed with SIGKILL leaves its socket file
				// behind. Binding without ever listening leaves the same
				// file; a closed listener would not, as a process forked
				// concurrently by another test briefly keeps it
				// listening.
				fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
				require.NoError(t, err)
				require.NoError(t, syscall.Bind(fd, &syscall.SockaddrUnix{Name: socket}))
				require.NoError(t, syscall.Close(fd))
				require.FileExists(t, socket)
				return socket
			},
		},
		{
			name: "refuses to displace a live server",
			socket: func(t *testing.T, dir string) string {
				socket := filepath.Join(dir, "live.sock")
				lis, err := Listen(socket)
				require.NoError(t, err)
				t.Cleanup(func() { lis.Close() })
				return socket
			},
			wantMsg: "workspace server already serving",
		},
		{
			name: "a directory that cannot be created",
			socket: func(t *testing.T, dir string) string {
				file := filepath.Join(dir, "file")
				require.NoError(t, os.WriteFile(file, nil, 0o600))
				return filepath.Join(file, "rune.sock")
			},
			wantMsg: "create socket dir",
		},
		{
			name: "a stale path that cannot be removed",
			socket: func(t *testing.T, dir string) string {
				socket := filepath.Join(dir, "stale.sock")
				require.NoError(t, os.MkdirAll(filepath.Join(socket, "child"), 0o700))
				return socket
			},
			wantMsg: "remove stale socket",
		},
		{
			name: "a path too long to bind",
			socket: func(t *testing.T, dir string) string {
				return filepath.Join(dir, strings.Repeat("s", 200)+".sock")
			},
			wantMsg: "listen",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			socket := tc.socket(t, workspacetest.ShortTempDir(t))

			lis, err := Listen(socket)
			if tc.wantMsg != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantMsg)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { lis.Close() })
			if tc.check != nil {
				tc.check(t, socket)
			}
		})
	}
}

// TestListenCannotInspectItsSocket needs a directory its own owner
// cannot search, which root always can.
func TestListenCannotInspectItsSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	t.Parallel()
	dir := filepath.Join(workspacetest.ShortTempDir(t), "sealed")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := Listen(filepath.Join(dir, "rune.sock"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat socket")
}

// flakyListener fails its accepts with errs, one each, until it runs
// out of them or is closed, and accepts for real after that.
type flakyListener struct {
	net.Listener
	mu     sync.Mutex
	errs   []error
	served int
	closed bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.errs) > 0 && !l.closed {
		err := l.errs[0]
		l.errs = l.errs[1:]
		l.served++
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

func (l *flakyListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return l.Listener.Close()
}

func (l *flakyListener) errorsServed() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.served
}

// One server owns every terminal of the machine, so running out of
// file descriptors for a moment must not end its accept loops.
func TestListenerRidesOutTransientAcceptErrors(t *testing.T) {
	t.Parallel()
	emfile := &net.OpError{
		Op: "accept", Net: "unix",
		Err: os.NewSyscallError("accept", syscall.EMFILE),
	}
	listen := func(t *testing.T, errs ...error) (net.Listener, *flakyListener, string) {
		t.Helper()
		path := filepath.Join(workspacetest.ShortTempDir(t), "s.sock")
		inner, err := net.Listen("unix", path)
		require.NoError(t, err)
		flaky := &flakyListener{Listener: inner, errs: errs}
		lis := newPatientListener(flaky)
		t.Cleanup(func() { _ = lis.Close() })
		return lis, flaky, path
	}

	t.Run("a transient failure is retried", func(t *testing.T) {
		t.Parallel()
		lis, _, path := listen(t, emfile, emfile)
		client, err := net.Dial("unix", path)
		require.NoError(t, err)
		defer client.Close()
		conn, err := lis.Accept()
		require.NoError(t, err)
		_ = conn.Close()
	})

	t.Run("any other failure is returned", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		lis, _, _ := listen(t, boom)
		_, err := lis.Accept()
		assert.ErrorIs(t, err, boom)
	})

	t.Run("closing ends the wait", func(t *testing.T) {
		t.Parallel()
		errs := make([]error, 100)
		for i := range errs {
			errs[i] = emfile
		}
		lis, flaky, _ := listen(t, errs...)
		accepted := make(chan error, 1)
		go debug.CapturePanicReport(func() {
			_, err := lis.Accept()
			accepted <- err
		})
		// By the ninth failure the listener backs off for a second.
		require.Eventually(t, func() bool { return flaky.errorsServed() >= 9 },
			5*time.Second, time.Millisecond)
		require.NoError(t, lis.Close())
		select {
		case err := <-accepted:
			assert.ErrorIs(t, err, net.ErrClosed)
		case <-time.After(300 * time.Millisecond):
			t.Fatal("Close did not end the backoff")
		}
	})
}

// TestConnectFails pins that a server that cannot be reached is an
// error the caller sees, never a connection that hangs forever.
func TestConnectFails(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		cfg     func(socket string) Config
		wantMsg string
	}{
		{
			name:    "without a socket",
			cfg:     func(string) Config { return Config{} },
			wantMsg: "socket is required",
		},
		{
			name: "when its command cannot be built",
			cfg: func(socket string) Config {
				return Config{
					Socket: socket,
					Command: func(string) (*exec.Cmd, error) {
						return nil, errors.New("no rune binary")
					},
				}
			},
			wantMsg: "build workspace server command: no rune binary",
		},
		{
			name: "when its command cannot start",
			cfg: func(socket string) Config {
				return Config{
					Socket: socket,
					Command: func(string) (*exec.Cmd, error) {
						return exec.Command(filepath.Join(filepath.Dir(socket), "missing")), nil
					},
				}
			},
			wantMsg: "start workspace server",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			socket := filepath.Join(workspacetest.ShortTempDir(t), "rune.sock")

			_, err := Connect(context.Background(), tc.cfg(socket))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestDialRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(workspacetest.ShortTempDir(t), "rune.sock")
	lis, err := Listen(socket)
	require.NoError(t, err)
	t.Cleanup(func() { lis.Close() })

	_, err = Dial(context.Background(), socket,
		grpc.WithPerRPCCredentials(secureOnlyCredentials{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "new client")
}

// secureOnlyCredentials cannot travel over the plaintext unix socket.
type secureOnlyCredentials struct{}

func (secureOnlyCredentials) GetRequestMetadata(
	context.Context, ...string,
) (map[string]string, error) {
	return nil, nil
}

func (secureOnlyCredentials) RequireTransportSecurity() bool { return true }
