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
	"fmt"
	"os"
	"os/user"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/runenet"
	"unstable.build/rune/internal/runenet/meshrpc"
	"unstable.build/rune/internal/workspace/workspaced"
)

// meshHost is the machine's network node as the workspace server hosts
// it: the node itself, the workspace server peers reach through it, and
// the RPC service the editors on this machine drive it with. The node
// lives and dies with the daemon, so the machine stays on the network
// while no editor window is open.
//
// The stack is always fully assembled, whatever the configuration
// says: `network.auto_join` only decides whether the daemon joins at
// start, so an editor's `network up` works later without a restart.
type meshHost struct {
	node   *runenet.Node
	gate   *networkGate
	server *meshrpc.Server
	// reload drops the API client's in-memory account token. The
	// editor's `login` and `logout` write to the shared data directory,
	// which this process only sees once it reads it again.
	reload func()
	// gated is false only when the node joins with credentials it was
	// given directly, which is a debug-build escape hatch: that mesh
	// is the developer's own, not the paid one, so the account is
	// never consulted.
	gated    bool
	autoJoin bool
	dataDir  string
	// observer is told, after the editors, that the node's status or
	// peers changed. It is what keeps the published status current.
	observer atomic.Pointer[func()]

	mu     sync.Mutex
	scheme schemeapi.Scheme
	opts   []runenet.ServeOption
	ws     *runenet.WorkspaceServer
	closed bool
	// removed is set once the account's machine list no longer has
	// this machine in it, and cleared when it registers again. The
	// editors learn of it through the Watch stream.
	removed atomic.Bool
}

// newMeshHost assembles the node and its entitlement gate. reload may
// be nil when the gate's client is not shared with another process. A
// malformed `network` config section falls back to defaults with a
// logged error rather than crippling the daemon: config validation is
// the user's mistake to surface, not a reason to run without terminals.
func newMeshHost(
	rootCfg config.Config, dataDir string, gate *networkGate, reload func(),
) *meshHost {
	if gate == nil {
		panic("newMeshHost: gate must not be nil")
	}
	cfg := networkConfigOrDefaults(rootCfg, dataDir)
	if reload == nil {
		reload = func() {}
	}
	ret := &meshHost{
		gate:     gate,
		reload:   reload,
		autoJoin: cfg.AutoJoin,
		dataDir:  dataDir,
		// A key configured out of band belongs to a debug build
		// driving a coordination server of its own, which the paid
		// mesh must not be mixed up with.
		gated: cfg.AuthKey == "",
	}
	if ret.gated {
		cfg.Credentials = ret.credentials
		cfg.OnNeedsLogin = ret.onNeedsLogin
	}
	ret.server = meshrpc.NewServer(ret, func(context.Context) bool {
		return ret.removed.Load()
	})
	cfg.OnChange = ret.notify
	ret.node = runenet.New(cfg)
	return ret
}

// notify tells everyone watching the node that it changed.
func (h *meshHost) notify() {
	h.server.Notify()
	if fn := h.observer.Load(); fn != nil {
		(*fn)()
	}
}

// observe has fn called whenever the node's status or peers change,
// on whichever goroutine saw the change.
func (h *meshHost) observe(fn func()) {
	h.observer.Store(&fn)
}

// statusTimeout bounds the node's answer for the published status,
// which is collected off a timer and must not hold it up.
const statusTimeout = 5 * time.Second

// status is the node's membership as the workspace server publishes
// it. A node that cannot answer is reported as not running with the
// reason, since whoever reads the file cannot ask again.
func (h *meshHost) status() workspaced.NetworkStatus {
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	st, err := h.node.Status(ctx)
	if err != nil {
		return workspaced.NetworkStatus{
			State:     runenet.StateNotRunning,
			LastError: err.Error(),
			Peers:     []workspaced.PeerStatus{},
		}
	}
	ret := workspaced.NetworkStatus{
		State:     st.State,
		Hostname:  st.Hostname,
		LoginName: st.LoginName,
		LastError: st.LastError,
		AuthURL:   st.AuthURL,
		Peers:     []workspaced.PeerStatus{},
	}
	if st.State == runenet.StateNotRunning {
		return ret
	}
	peers, err := h.node.Peers(ctx)
	if err != nil {
		return ret
	}
	for _, p := range peers {
		ret.Peers = append(ret.Peers, workspaced.PeerStatus{
			Hostname: p.Hostname,
			DNSName:  p.DNSName,
			OS:       p.OS,
			Online:   p.Online,
		})
	}
	return ret
}

// credentials satisfies runenet.CredentialsFunc with the account token
// as it is on disk right now, so a login or logout performed by an
// editor is honoured without restarting the daemon.
func (h *meshHost) credentials(ctx context.Context) (string, string, error) {
	h.reload()
	return h.gate.credentials(ctx)
}

// needsLoginTimeout bounds the account round-trip a logout triggers.
const needsLoginTimeout = 30 * time.Second

// onNeedsLogin runs when the coordination server stops accepting this
// machine's node key. Two very different things look like that from
// here and only the account's machine list tells them apart: a node
// key that reached the end of its lifetime, which is re-minted without
// bothering anyone, and a machine the account removed on purpose,
// which the editors are told about through the Watch stream.
func (h *meshHost) onNeedsLogin() {
	go debug.CapturePanicReport(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), needsLoginTimeout)
		defer cancel()
		st, err := h.node.Status(ctx)
		if err != nil || st.MachineID == "" {
			// A machine that never registered has no removal to
			// report and no key worth re-minting.
			return
		}
		h.handleNeedsLogin(ctx, st.MachineID)
	})
}

func (h *meshHost) handleNeedsLogin(ctx context.Context, machineID string) {
	h.reload()
	machines, err := h.gate.machines(ctx)
	if err != nil {
		// Without the list the two cases are indistinguishable, and
		// claiming the machine was removed when the account server is
		// merely unreachable would be worse than saying nothing.
		log.Warnf("network: could not check this machine's registration: %v", err)
		return
	}
	if machineRemoved(machineID, machines) {
		h.removed.Store(true)
		h.notify()
		return
	}
	if err := h.Up(ctx); err != nil {
		log.Warnf("could not re-register machine on the network: %v", err)
	}
}

// serveTo names the scheme peers are served, and opts what else is
// served next to it. It is the workspace server's own, terminal-owning
// scheme, so a peer's rune:// workspace gets the same persistent
// terminals a local client gets. The scheme stays owned by the caller.
func (h *meshHost) serveTo(scheme schemeapi.Scheme, opts ...runenet.ServeOption) {
	h.mu.Lock()
	h.scheme = scheme
	h.opts = opts
	h.mu.Unlock()
	if err := h.ensureServing(); err != nil &&
		!errors.Is(err, runenet.ErrNotStarted) {
		log.Warnf("network: %v", err)
	}
}

// ensureServing serves the scheme to peers once the node is up. It is
// idempotent, so every path that brings the node online calls it.
func (h *meshHost) ensureServing() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.ws != nil || h.scheme == nil {
		return nil
	}
	ws, err := servePeers(h.node, h.scheme, h.dataDir, h.opts...)
	if err != nil {
		return fmt.Errorf("could not serve workspaces on the network: %w", err)
	}
	h.ws = ws
	return nil
}

// servePeers exposes scheme to the machines on the network. Only
// machines owned by the same account get that far; see
// runenet.PeerAuthorizer.
//
// dataDir is advertised to peers as this instance's install root, so
// it is resolved to an absolute path here: the peer consumes it as a
// path on this host, not relative to its own process.
func servePeers(
	node *runenet.Node, scheme schemeapi.Scheme, dataDir string,
	opts ...runenet.ServeOption,
) (*runenet.WorkspaceServer, error) {
	dataDir, err := workspaceapi.ExpandPath(dataDir, user.Current, os.Getwd)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir %s: %w", dataDir, err)
	}
	return runenet.ServeWorkspace(node, scheme, dataDir, opts...)
}

// startAutoJoin joins the mesh off the start path when the user asked
// for it. A signed-out or unentitled account simply does not join,
// and is logged as such rather than as a failure: the prompt that
// fixes it belongs to an editor, when the user asks for the network
// explicitly.
func (h *meshHost) startAutoJoin() {
	autoJoin(h.autoJoin, h.Join)
}

// Status satisfies meshrpc.Host.
func (h *meshHost) Status(ctx context.Context) (runenet.Status, error) {
	return h.node.Status(ctx)
}

// Peers satisfies meshrpc.Host.
func (h *meshHost) Peers(ctx context.Context) ([]runenet.Peer, error) {
	return h.node.Peers(ctx)
}

// Join satisfies meshrpc.Host: it brings the node online and serves
// this machine's workspaces to its peers. It is idempotent, so a
// machine that could not join at start joins on the first `network up`
// or rune:// open that follows.
func (h *meshHost) Join(ctx context.Context) error {
	if err := h.node.Join(ctx); err != nil {
		return err
	}
	return h.ensureServing()
}

// Up satisfies meshrpc.Host.
func (h *meshHost) Up(ctx context.Context) error {
	if err := h.node.Up(ctx); err != nil {
		return err
	}
	h.registered()
	return h.ensureServing()
}

// Rejoin satisfies meshrpc.Host.
func (h *meshHost) Rejoin(ctx context.Context) error {
	if err := h.node.Rejoin(ctx); err != nil {
		return err
	}
	h.registered()
	return h.ensureServing()
}

// registered clears a removal once the machine is a member again.
func (h *meshHost) registered() {
	if h.removed.Swap(false) {
		h.notify()
	}
}

// Down satisfies meshrpc.Host.
func (h *meshHost) Down(ctx context.Context) error {
	return h.node.Down(ctx)
}

// Close leaves the mesh and stops serving workspaces to peers.
func (h *meshHost) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	ws := h.ws
	h.ws = nil
	h.mu.Unlock()

	var errs []error
	if ws != nil {
		errs = append(errs, ws.Close())
	}
	// The node outlives Close so a request racing shutdown reads a
	// closed node rather than a nil one.
	errs = append(errs, h.node.Close())
	return errors.Join(errs...)
}
