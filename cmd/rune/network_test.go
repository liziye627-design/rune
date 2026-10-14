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

package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"google.golang.org/grpc"
	"unstable.build/rune/auth"
	"unstable.build/rune/cmd/rune/ide/apiclient"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/runenet"
	"unstable.build/rune/internal/runenet/meshrpc"
	"unstable.build/rune/internal/workspace/workspaceserver"
)

// fakeMeshHost stands in for the node the workspace server hosts.
type fakeMeshHost struct {
	mu      sync.Mutex
	status  runenet.Status
	peers   []runenet.Peer
	upErr   error
	calls   []string
	removed atomic.Bool
}

func (h *fakeMeshHost) record(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, name)
}

func (h *fakeMeshHost) called() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *fakeMeshHost) Status(context.Context) (runenet.Status, error) {
	h.record("status")
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status, nil
}

func (h *fakeMeshHost) Peers(context.Context) ([]runenet.Peer, error) {
	h.record("peers")
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peers, nil
}

func (h *fakeMeshHost) Join(context.Context) error   { h.record("join"); return nil }
func (h *fakeMeshHost) Up(context.Context) error     { h.record("up"); return h.upErr }
func (h *fakeMeshHost) Rejoin(context.Context) error { h.record("rejoin"); return nil }
func (h *fakeMeshHost) Down(context.Context) error   { h.record("down"); return nil }

// countingPrompter counts prompts off the event loop, where the watch
// delivers them.
type countingPrompter struct {
	prompts atomic.Int32
}

func (p *countingPrompter) prompter() *networkPrompter {
	return &networkPrompter{
		scheduleNextTick: func(func()) bool { p.prompts.Add(1); return true },
	}
}

// serveMeshHost serves host on the workspace server socket of a fresh
// data directory, the way `rune --headless` does, and returns the
// editor-side network reaching it along with the server.
func serveMeshHost(
	t *testing.T, api *stubNetworkAPI, host *fakeMeshHost,
) (*network, *meshrpc.Server) {
	t.Helper()
	// Not t.TempDir(): socket paths must fit sockaddr_un.
	dataDir, err := os.MkdirTemp("/tmp", "runenet")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })

	lis, err := workspaceserver.Listen(workspaceserver.SocketPath(dataDir))
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	server := meshrpc.NewServer(host, func(context.Context) bool {
		return host.removed.Load()
	})
	server.Register(grpcServer)
	go debug.CapturePanicReport(func() { _ = grpcServer.Serve(lis) })
	t.Cleanup(grpcServer.Stop)

	net := newNetwork(config.NopConfig(), dataDir, newNetworkGate(api), nil)
	t.Cleanup(func() { _ = net.Close() })
	return net, server
}

func signedIn() *stubNetworkAPI {
	return &stubNetworkAPI{
		user:     auth.RPCUser{Role: auth.RoleUser},
		signedIn: true,
		machines: []apiclient.Machine{{ID: "1", Hostname: "laptop"}},
	}
}

// The `network` command's answers come from the workspace server, and
// the sentinels the prompt branches on survive the hop.
func TestGatedNetworkAgainstServer(t *testing.T) {
	t.Run("status and peers are the server's", func(t *testing.T) {
		host := &fakeMeshHost{
			status: runenet.Status{Hostname: "carbon", State: runenet.StateRunning, MachineID: "1"},
			peers:  []runenet.Peer{{Hostname: "studio", Online: true}},
		}
		net, _ := serveMeshHost(t, signedIn(), host)
		counting := &countingPrompter{}
		gated := gatedNetwork{n: net, prompter: counting.prompter()}

		st, err := gated.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, host.status, st)
		peers, err := gated.Peers(context.Background())
		require.NoError(t, err)
		assert.Equal(t, host.peers, peers)
		assert.Zero(t, counting.prompts.Load())
	})

	t.Run("a failed up prompts with the server's reason", func(t *testing.T) {
		host := &fakeMeshHost{
			status: runenet.Status{State: runenet.StateNotRunning},
			upErr:  runenet.ErrNotAuthenticated,
		}
		net, _ := serveMeshHost(t, signedIn(), host)
		counting := &countingPrompter{}
		gated := gatedNetwork{n: net, prompter: counting.prompter()}

		err := gated.Up(context.Background())
		require.Error(t, err)
		assert.True(t, errors.Is(err, runenet.ErrNotAuthenticated), "got %v", err)
		assert.Equal(t, int32(1), counting.prompts.Load())
		assert.Contains(t, host.called(), "up")
	})

	t.Run("up re-registers a machine the account dropped", func(t *testing.T) {
		host := &fakeMeshHost{
			status: runenet.Status{State: runenet.StateRunning, MachineID: "9"},
		}
		net, _ := serveMeshHost(t, signedIn(), host)
		gated := gatedNetwork{n: net, prompter: (&countingPrompter{}).prompter()}

		require.NoError(t, gated.Up(context.Background()))
		assert.Contains(t, host.called(), "rejoin")
		assert.NotContains(t, host.called(), "up")
	})

	t.Run("down and logout reach the server", func(t *testing.T) {
		host := &fakeMeshHost{status: runenet.Status{State: runenet.StateRunning}}
		net, _ := serveMeshHost(t, signedIn(), host)
		gated := gatedNetwork{n: net, prompter: (&countingPrompter{}).prompter()}

		require.NoError(t, gated.Down(context.Background()))
		net.loggedOut(context.Background())
		assert.Equal(t, []string{"down", "down"}, host.called())
	})

	t.Run("signed out is told before the server is asked", func(t *testing.T) {
		host := &fakeMeshHost{}
		net, _ := serveMeshHost(t, &stubNetworkAPI{}, host)
		counting := &countingPrompter{}
		gated := gatedNetwork{n: net, prompter: counting.prompter()}

		_, err := gated.Status(context.Background())
		assert.True(t, errors.Is(err, runenet.ErrNotAuthenticated), "got %v", err)
		assert.True(t, errors.Is(gated.Up(context.Background()),
			runenet.ErrNotAuthenticated))
		assert.Equal(t, int32(2), counting.prompts.Load())
		assert.Empty(t, host.called())
	})
}

// A removal the workspace server notices is shown once, and shown
// again only after the machine was registered and removed again.
func TestNetworkWatchPromptsOnRemoval(t *testing.T) {
	host := &fakeMeshHost{status: runenet.Status{State: runenet.StateRunning}}
	net, server := serveMeshHost(t, signedIn(), host)
	counting := &countingPrompter{}
	net.setPrompter(counting.prompter())
	watchDone := make(chan struct{})
	go debug.CapturePanicReport(func() {
		defer close(watchDone)
		net.watch()
	})

	prompted := func(n int32) {
		t.Helper()
		assert.Eventually(t, func() bool { return counting.prompts.Load() == n },
			5*time.Second, 10*time.Millisecond)
	}

	host.removed.Store(true)
	server.Notify()
	prompted(1)

	server.Notify()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), counting.prompts.Load(), "a removal is shown once")

	host.removed.Store(false)
	server.Notify()
	// The server coalesces notifications, so the registration has to
	// be seen before the next removal or the two collapse into one.
	time.Sleep(100 * time.Millisecond)
	host.removed.Store(true)
	server.Notify()
	prompted(2)

	require.NoError(t, net.Close())
	select {
	case <-watchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop with the network")
	}
}

// A network nobody serves is not an error for the parts that only
// listen: the watch waits for a server, and logout has nothing to take
// down.
func TestNetworkWithoutServer(t *testing.T) {
	dataDir, err := os.MkdirTemp("/tmp", "runenet")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	net := newNetwork(config.NopConfig(), dataDir, newNetworkGate(signedIn()), nil)
	t.Cleanup(func() { _ = net.Close() })

	assert.NoError(t, net.down(context.Background()))
	_, err = net.running(context.Background())
	assert.Error(t, err)
	err = net.join(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no command to start one")
}

// Reaching the workspace server may mean starting it and waiting for
// it to serve. Nothing else the network does may wait behind that, and
// quitting the editor abandons it.
func TestNetworkConnectingHoldsNothingUp(t *testing.T) {
	dataDir, err := os.MkdirTemp("/tmp", "runenet")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	net := newNetwork(config.NopConfig(), dataDir, newNetworkGate(signedIn()), nil)
	t.Cleanup(func() { _ = net.Close() })

	var connects atomic.Int32
	entered := make(chan struct{})
	net.connectServer = func(
		ctx context.Context, _ workspaceserver.Config,
	) (*grpc.ClientConn, error) {
		if connects.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	errs := make(chan error, 2)
	for range 2 {
		go debug.CapturePanicReport(func() {
			_, err := net.connect(context.Background())
			errs <- err
		})
	}
	<-entered

	closed := make(chan struct{})
	go debug.CapturePanicReport(func() {
		net.setPrompter(nil)
		_ = net.Close()
		close(closed)
	})
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing waited on a connection being made")
	}
	for range 2 {
		select {
		case err := <-errs:
			assert.ErrorIs(t, err, errNetworkClosed)
		case <-time.After(5 * time.Second):
			t.Fatal("closing did not abandon the connection being made")
		}
	}
	assert.EqualValues(t, 1, connects.Load(),
		"callers share the connection being made")
}

// fakeLoginClient is the account client the login shell runs on, with
// a sign-in that succeeds at once.
type fakeLoginClient struct {
	loginErr error
}

func (c fakeLoginClient) Login(context.Context) apiclient.LoginSession {
	urlCh := make(chan *url.URL)
	close(urlCh)
	done := make(chan error, 1)
	done <- c.loginErr
	close(done)
	return apiclient.LoginSession{URL: urlCh, Done: done}
}

func (fakeLoginClient) Logout(context.Context) error { return nil }

func (fakeLoginClient) AccountStatus(context.Context) (auth.RPCUser, bool, error) {
	return auth.RPCUser{}, true, nil
}

// Signing in puts a machine the user wants on the network on it, and
// signing out takes it off: the node holds credentials minted for the
// account that just left.
func TestNetworkFollowsLoginAndLogout(t *testing.T) {
	t.Run("login joins when auto_join is on", func(t *testing.T) {
		host := &fakeMeshHost{}
		net, _ := serveMeshHost(t, signedIn(), host)
		net.autoJoin = true
		client := net.loginClient(fakeLoginClient{})

		session := client.Login(context.Background())
		err, ok := <-session.Done
		require.True(t, ok)
		require.NoError(t, err)
		assert.Eventually(t, func() bool {
			return slices.Contains(host.called(), "join")
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("a failed login joins nothing", func(t *testing.T) {
		host := &fakeMeshHost{}
		net, _ := serveMeshHost(t, signedIn(), host)
		net.autoJoin = true
		client := net.loginClient(fakeLoginClient{loginErr: errors.New("denied")})

		session := client.Login(context.Background())
		err, ok := <-session.Done
		require.True(t, ok)
		require.Error(t, err)
		// The client decides on the join before it reports the outcome.
		assert.Empty(t, host.called())
	})

	t.Run("logout leaves the network", func(t *testing.T) {
		host := &fakeMeshHost{}
		net, _ := serveMeshHost(t, signedIn(), host)
		client := net.loginClient(fakeLoginClient{})

		require.NoError(t, client.Logout(context.Background()))
		assert.Equal(t, []string{"down"}, host.called())
	})
}
