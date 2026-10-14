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

package meshrpc

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/runenet"
)

type fakeHost struct {
	mu        sync.Mutex
	status    runenet.Status
	statusErr error
	peers     []runenet.Peer
	peersErr  error
	joinErr   error
	upErr     error
	rejoinErr error
	downErr   error
	calls     []string
}

func (h *fakeHost) record(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, name)
}

func (h *fakeHost) Status(context.Context) (runenet.Status, error) {
	h.record("status")
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status, h.statusErr
}

func (h *fakeHost) Peers(context.Context) ([]runenet.Peer, error) {
	h.record("peers")
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peers, h.peersErr
}

func (h *fakeHost) Join(context.Context) error   { h.record("join"); return h.joinErr }
func (h *fakeHost) Up(context.Context) error     { h.record("up"); return h.upErr }
func (h *fakeHost) Rejoin(context.Context) error { h.record("rejoin"); return h.rejoinErr }
func (h *fakeHost) Down(context.Context) error   { h.record("down"); return h.downErr }

func (h *fakeHost) setStatus(st runenet.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = st
}

func (h *fakeHost) called() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// serve runs server on an in-memory listener and returns a client
// connected to it.
func serve(t *testing.T, server *Server) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	server.Register(grpcServer)
	go debug.CapturePanicReport(func() { _ = grpcServer.Serve(lis) })
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///mesh",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return NewClient(conn)
}

func TestClientServerRoundTrip(t *testing.T) {
	host := &fakeHost{
		status: runenet.Status{
			Hostname: "carbon",
			State:    runenet.StateRunning,
			Addrs:    []netip.Addr{netip.MustParseAddr("100.64.0.1")},
		},
		peers: []runenet.Peer{{Hostname: "studio", Online: true}},
	}
	client := serve(t, NewServer(host, nil))
	ctx := context.Background()

	st, err := client.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, host.status, st)

	peers, err := client.Peers(ctx)
	require.NoError(t, err)
	assert.Equal(t, host.peers, peers)

	require.NoError(t, client.Join(ctx))
	require.NoError(t, client.Up(ctx))
	require.NoError(t, client.Rejoin(ctx))
	require.NoError(t, client.Down(ctx))
	assert.Equal(t, []string{"status", "peers", "join", "up", "rejoin", "down"},
		host.called())
}

// What the daemon's node answers must read the same on the editor's
// side of the socket, or every errors.Is in the editor goes quiet.
func TestClientServerErrors(t *testing.T) {
	host := &fakeHost{
		statusErr: errors.New("backend gone"),
		peersErr:  runenet.ErrNotStarted,
		joinErr:   runenet.ErrNotAuthenticated,
		upErr:     runenet.ErrMachineLimit,
		rejoinErr: runenet.ErrSubscriptionRequired,
		downErr:   runenet.ErrNotStarted,
	}
	client := serve(t, NewServer(host, nil))
	ctx := context.Background()

	_, err := client.Status(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backend gone")

	_, err = client.Peers(ctx)
	assert.True(t, errors.Is(err, runenet.ErrNotStarted), "got %v", err)
	assert.True(t, errors.Is(client.Join(ctx), runenet.ErrNotAuthenticated))
	assert.True(t, errors.Is(client.Up(ctx), runenet.ErrMachineLimit))
	assert.True(t, errors.Is(client.Rejoin(ctx), runenet.ErrSubscriptionRequired))
	assert.True(t, errors.Is(client.Down(ctx), runenet.ErrNotStarted))
}

func TestWatch(t *testing.T) {
	recv := func(t *testing.T, ch <-chan Change) Change {
		t.Helper()
		select {
		case ev, ok := <-ch:
			require.True(t, ok, "stream ended")
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
			return Change{}
		}
	}

	t.Run("sends the current picture on subscribe", func(t *testing.T) {
		host := &fakeHost{
			status: runenet.Status{Hostname: "carbon", State: runenet.StateRunning},
			peers:  []runenet.Peer{{Hostname: "studio"}},
		}
		client := serve(t, NewServer(host, nil))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch, err := client.Watch(ctx)
		require.NoError(t, err)
		ev := recv(t, ch)
		assert.Equal(t, host.status, ev.Status)
		assert.Equal(t, host.peers, ev.Peers)
		assert.False(t, ev.MachineRemoved)
	})

	t.Run("sends one per notification with the new picture", func(t *testing.T) {
		host := &fakeHost{status: runenet.Status{State: runenet.StateNotRunning}}
		server := NewServer(host, nil)
		client := serve(t, server)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch, err := client.Watch(ctx)
		require.NoError(t, err)
		assert.Equal(t, runenet.StateNotRunning, recv(t, ch).Status.State)

		host.setStatus(runenet.Status{State: runenet.StateRunning})
		server.Notify()
		assert.Equal(t, runenet.StateRunning, recv(t, ch).Status.State)
	})

	t.Run("reports a removed machine", func(t *testing.T) {
		host := &fakeHost{status: runenet.Status{State: runenet.StateRunning}}
		var removed bool
		var mu sync.Mutex
		server := NewServer(host, func(context.Context) bool {
			mu.Lock()
			defer mu.Unlock()
			return removed
		})
		client := serve(t, server)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch, err := client.Watch(ctx)
		require.NoError(t, err)
		assert.False(t, recv(t, ch).MachineRemoved)

		mu.Lock()
		removed = true
		mu.Unlock()
		server.Notify()
		assert.True(t, recv(t, ch).MachineRemoved)
	})

	t.Run("a node without peers is still watchable", func(t *testing.T) {
		host := &fakeHost{
			status:   runenet.Status{State: runenet.StateNotRunning},
			peersErr: runenet.ErrNotStarted,
		}
		client := serve(t, NewServer(host, nil))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch, err := client.Watch(ctx)
		require.NoError(t, err)
		assert.Empty(t, recv(t, ch).Peers)
	})

	t.Run("notifying with nobody watching does not block", func(t *testing.T) {
		server := NewServer(&fakeHost{}, nil)
		done := make(chan struct{})
		go debug.CapturePanicReport(func() {
			defer close(done)
			for range 100 {
				server.Notify()
			}
		})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Notify blocked")
		}
	})

	t.Run("closes the channel when the watcher gives up", func(t *testing.T) {
		client := serve(t, NewServer(&fakeHost{}, nil))
		ctx, cancel := context.WithCancel(context.Background())

		ch, err := client.Watch(ctx)
		require.NoError(t, err)
		recv(t, ch)
		cancel()
		select {
		case _, ok := <-ch:
			assert.False(t, ok)
		case <-time.After(5 * time.Second):
			t.Fatal("channel never closed")
		}
	})
}

func TestNewServerPanicsWithoutHost(t *testing.T) {
	assert.Panics(t, func() { NewServer(nil, nil) })
}
