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

package meshdial

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/runenet"
)

// fakePeers hands out one end of a pipe per dial and keeps the other,
// standing in for the peers a node would reach over the mesh.
type fakePeers struct {
	mu      sync.Mutex
	err     error
	dials   []string
	remotes chan net.Conn
	block   chan struct{}
}

func newFakePeers() *fakePeers {
	return &fakePeers{remotes: make(chan net.Conn, 8)}
}

func (f *fakePeers) Dial(ctx context.Context, peer string) (net.Conn, error) {
	return f.dial(ctx, "dial:"+peer)
}

func (f *fakePeers) DialAttachment(ctx context.Context, peer string) (net.Conn, error) {
	return f.dial(ctx, "attach:"+peer)
}

func (f *fakePeers) dial(ctx context.Context, what string) (net.Conn, error) {
	f.mu.Lock()
	f.dials = append(f.dials, what)
	err := f.err
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	local, remote := net.Pipe()
	f.remotes <- remote
	return local, nil
}

func (f *fakePeers) dialed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dials...)
}

// serve runs Serve on a unix socket and returns a Dialer for it.
func serve(t *testing.T, peers PeerDialer) Dialer {
	t.Helper()
	// Not t.TempDir(): socket paths must fit sockaddr_un.
	dir, err := os.MkdirTemp("/tmp", "meshdial")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "net.sock")
	lis, err := net.Listen("unix", socket)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go debug.CapturePanicReport(func() { done <- Serve(ctx, lis, peers) })
	t.Cleanup(func() {
		cancel()
		_ = lis.Close()
		assert.NoError(t, <-done)
	})
	return Dialer{Socket: socket}
}

func TestDialProxiesBothWays(t *testing.T) {
	for _, tc := range []struct {
		name string
		dial func(Dialer, context.Context, string) (net.Conn, error)
		want string
	}{
		{"workspace port", Dialer.Dial, "dial:studio"},
		{"attachment port", Dialer.DialAttachment, "attach:studio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peers := newFakePeers()
			dialer := serve(t, peers)

			conn, err := tc.dial(dialer, context.Background(), "studio")
			require.NoError(t, err)
			defer conn.Close()
			remote := <-peers.remotes
			defer remote.Close()
			assert.Equal(t, []string{tc.want}, peers.dialed())

			go debug.CapturePanicReport(func() { _, _ = io.WriteString(conn, "hello peer") })
			assert.Equal(t, "hello peer", readN(t, remote, len("hello peer")))
			go debug.CapturePanicReport(func() { _, _ = io.WriteString(remote, "hello client") })
			assert.Equal(t, "hello client", readN(t, conn, len("hello client")))
		})
	}
}

// Every sentinel the reconnect loop or the prompt branches on has to
// come back out of the socket as itself.
func TestDialRestoresSentinels(t *testing.T) {
	for _, sentinel := range []error{
		runenet.ErrNotStarted,
		runenet.ErrNotAuthenticated,
		runenet.ErrSubscriptionRequired,
		runenet.ErrMachineLimit,
		runenet.ErrPeerNotFound,
		runenet.ErrMachineRemoved,
	} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			peers := newFakePeers()
			peers.err = fmt.Errorf("%q: %w", "studio", sentinel)
			dialer := serve(t, peers)

			_, err := dialer.Dial(context.Background(), "studio")
			require.Error(t, err)
			assert.True(t, errors.Is(err, sentinel), "got %v", err)
			assert.Equal(t, peers.err.Error(), err.Error())
		})
	}

	t.Run("other errors keep their message", func(t *testing.T) {
		peers := newFakePeers()
		peers.err = errors.New("no route\nto host")
		dialer := serve(t, peers)

		_, err := dialer.Dial(context.Background(), "studio")
		require.Error(t, err)
		assert.Equal(t, "no route to host", err.Error())
	})
}

func TestClosingOneSideClosesTheOther(t *testing.T) {
	t.Run("client hangs up", func(t *testing.T) {
		peers := newFakePeers()
		dialer := serve(t, peers)
		conn, err := dialer.Dial(context.Background(), "studio")
		require.NoError(t, err)
		remote := <-peers.remotes

		require.NoError(t, conn.Close())
		_, err = remote.Read(make([]byte, 1))
		assert.Error(t, err)
	})

	t.Run("peer hangs up", func(t *testing.T) {
		peers := newFakePeers()
		dialer := serve(t, peers)
		conn, err := dialer.Dial(context.Background(), "studio")
		require.NoError(t, err)
		remote := <-peers.remotes

		require.NoError(t, remote.Close())
		_, err = conn.Read(make([]byte, 1))
		assert.Error(t, err)
	})
}

func TestDialRejectsBadInput(t *testing.T) {
	dialer := serve(t, newFakePeers())
	for _, peer := range []string{"", "two words", "new\nline"} {
		_, err := dialer.Dial(context.Background(), peer)
		assert.Error(t, err, "peer %q", peer)
	}
	_, err := Dialer{}.Dial(context.Background(), "studio")
	assert.Error(t, err)
}

func TestDialHonoursContextDuringHandshake(t *testing.T) {
	peers := newFakePeers()
	peers.block = make(chan struct{})
	defer close(peers.block)
	dialer := serve(t, peers)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := dialer.Dial(ctx, "studio")
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
}

func TestServeRejectsMalformedHandshake(t *testing.T) {
	dialer := serve(t, newFakePeers())
	for _, line := range []string{"OPEN studio\n", "DIAL\n", "DIAL \n"} {
		conn, err := net.Dial("unix", dialer.Socket)
		require.NoError(t, err)
		_, err = io.WriteString(conn, line)
		require.NoError(t, err)
		reply := make([]byte, 4)
		_, err = io.ReadFull(conn, reply)
		require.NoError(t, err)
		assert.Equal(t, "ERR ", string(reply), "line %q", line)
		_ = conn.Close()
	}
}

func TestServePanicsWithoutDialer(t *testing.T) {
	assert.Panics(t, func() { _ = Serve(context.Background(), nil, nil) })
}

// A peer may speak first, as an HTTP/2 server does with its SETTINGS
// frame, so its first bytes can share a read with the reply.
func TestDialKeepsPeerBytesThatArriveWithTheReply(t *testing.T) {
	socket := fakeServer(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = io.WriteString(conn, "OK\nSETTINGS")
	})

	conn, err := Dialer{Socket: socket}.Dial(context.Background(), "studio")
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, "SETTINGS", readN(t, conn, len("SETTINGS")))
}

func TestDialRejectsAnOverlongReply(t *testing.T) {
	socket := fakeServer(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write(bytes.Repeat([]byte("x"), 2*maxLine))
		<-time.After(handshakeTimeout)
	})

	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout/2)
	defer cancel()
	_, err := Dialer{Socket: socket}.Dial(ctx, "studio")
	require.Error(t, err)
	assert.NotErrorIs(t, err, context.DeadlineExceeded,
		"an overlong reply must fail on its length, not wait for more")
}

func TestServeRejectsAnOverlongHandshake(t *testing.T) {
	dialer := serve(t, newFakePeers())
	conn, err := net.Dial("unix", dialer.Socket)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write(bytes.Repeat([]byte("x"), 2*maxLine))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(handshakeTimeout/2)))
	reply, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err, "an overlong line must be refused, not read on")
	assert.Equal(t, "ERR NONE handshake line too long\n", reply)
}

// fakeServer listens on a unix socket and runs handle for each
// connection, standing in for a server that speaks the protocol badly.
func fakeServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "meshdial")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "net.sock")
	lis, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	go debug.CapturePanicReport(func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go debug.CapturePanicReport(func() {
				defer conn.Close()
				handle(conn)
			})
		}
	})
	return socket
}

func readN(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	require.NoError(t, err)
	return string(buf)
}
