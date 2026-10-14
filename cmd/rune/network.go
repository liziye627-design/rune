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
	"net"
	"os/exec"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	"unstable.build/rune/cmd/rune/ide/apiclient"
	"unstable.build/rune/cmd/rune/loginshell"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/ide"
	"unstable.build/rune/internal/ide/console/networkconsole"
	"unstable.build/rune/internal/runenet"
	"unstable.build/rune/internal/runenet/meshdial"
	"unstable.build/rune/internal/runenet/meshrpc"
	"unstable.build/rune/internal/workspace/workspacerune"
	"unstable.build/rune/internal/workspace/workspaceserver"
)

// network is the editor's side of the machine's network node, which
// the workspace server hosts so the machine stays on the network while
// no editor window is open. The editor drives the node over the
// server's socket and reaches peers through the server's mesh socket;
// the entitlement check, the account's machine list and every prompt
// stay here, on the editor's own account client.
//
// The connection to the server is made on first use and starting the
// server is part of making it, so a boot with `network.auto_join` on
// puts the machine on the network before any terminal is opened.
type network struct {
	gate *networkGate
	// gated is false only when the node joins with credentials it was
	// given directly, which is a debug-build escape hatch: that mesh
	// is the developer's own, not the paid one, so the account is
	// never consulted.
	gated    bool
	autoJoin bool
	dataDir  string
	// command starts the workspace server when none is running. Only
	// rune knows it is rune, so a host that merely embeds the IDE
	// leaves it empty and reaches a running server only.
	command []string
	dialer  meshdial.Dialer
	// connectServer is [workspaceserver.Connect]; tests stand in for it.
	connectServer func(context.Context, workspaceserver.Config) (*grpc.ClientConn, error)

	mu     sync.Mutex
	conn   *grpc.ClientConn
	client *meshrpc.Client
	closed bool
	// connecting is closed once the connection being made settles. It
	// is made without mu held: it may start the server and wait for it
	// to serve, and nothing else may wait behind that.
	connecting chan struct{}
	// prompter is only there once the IDE exists; a push that lands
	// before then has nowhere to be shown.
	prompter *networkPrompter

	watchCtx    context.Context
	cancelWatch context.CancelFunc
}

// newNetwork assembles the editor's view of the network. command is
// what starts the workspace server; see [workspaceServerCommand]. A
// malformed `network` config section falls back to defaults with a
// logged error rather than crippling the stack: config validation is
// the user's mistake to surface, not a reason to run partially
// constructed.
func newNetwork(
	rootCfg config.Config, dataDir string, gate *networkGate, command []string,
) *network {
	if gate == nil {
		panic("newNetwork: gate must not be nil")
	}
	cfg := networkConfigOrDefaults(rootCfg, dataDir)
	ret := &network{
		gate:          gate,
		autoJoin:      cfg.AutoJoin,
		dataDir:       dataDir,
		command:       command,
		dialer:        meshdial.Dialer{Socket: workspaceserver.NetSocketPath(dataDir)},
		connectServer: workspaceserver.Connect,
		// A key configured out of band belongs to a debug build
		// driving a coordination server of its own, which the paid
		// mesh must not be mixed up with.
		gated: cfg.AuthKey == "",
	}
	ret.watchCtx, ret.cancelWatch = context.WithCancel(context.Background())
	return ret
}

// networkConfigOrDefaults is [networkConfig], falling back to the
// defaults with a logged error.
func networkConfigOrDefaults(rootCfg config.Config, dataDir string) runenet.Config {
	cfg, err := networkConfig(rootCfg, dataDir)
	if err != nil {
		log.Errorf("network: %v (using defaults)", err)
		cfg, _ = runenet.FromConfig(config.NopConfig(), dataDir)
	}
	return cfg
}

func networkConfig(rootCfg config.Config, dataDir string) (runenet.Config, error) {
	cfg, err := rootCfg.GetConfig("network")
	if errors.Is(err, config.ErrNotFound) {
		cfg = config.NopConfig()
	} else if err != nil {
		return runenet.Config{}, fmt.Errorf("get 'network' section from config: %w", err)
	}
	return runenet.FromConfig(cfg, dataDir)
}

var errNetworkClosed = errors.New("network: editor is shutting down")

// connect returns the client of the workspace server, starting the
// server when none is running. A connection the server went away from
// is dropped so the next call starts a replacement rather than waiting
// on a socket nobody serves.
func (n *network) connect(ctx context.Context) (*meshrpc.Client, error) {
	return n.clientFor(ctx, true)
}

// running returns the client of a workspace server already running,
// and fails fast when there is none: the caller only wants to hear
// from a server, not to start one.
func (n *network) running(ctx context.Context) (*meshrpc.Client, error) {
	return n.clientFor(ctx, false)
}

func (n *network) clientFor(ctx context.Context, start bool) (*meshrpc.Client, error) {
	for {
		n.mu.Lock()
		if n.closed {
			n.mu.Unlock()
			return nil, errNetworkClosed
		}
		if n.conn != nil && n.conn.GetState() != connectivity.TransientFailure {
			client := n.client
			n.mu.Unlock()
			return client, nil
		}
		if n.conn != nil {
			_ = n.conn.Close()
			n.conn, n.client = nil, nil
		}
		wait := n.connecting
		if wait == nil {
			n.connecting = make(chan struct{})
		}
		n.mu.Unlock()
		if wait == nil {
			return n.connectOnce(ctx, start)
		}
		// The connection being made may not start a server, or may
		// fail for its caller's reasons, so this caller looks again.
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// connectOnce makes the connection clientFor found missing, and owns
// n.connecting until it settles.
func (n *network) connectOnce(ctx context.Context, start bool) (*meshrpc.Client, error) {
	cfg := workspaceserver.Config{Socket: workspaceserver.SocketPath(n.dataDir)}
	if start && len(n.command) > 0 {
		cfg.Command = func(string) (*exec.Cmd, error) {
			return exec.Command(n.command[0], n.command[1:]...), nil
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Quitting the editor abandons a connection still being made.
	defer context.AfterFunc(n.watchCtx, cancel)()
	conn, err := n.connectServer(ctx, cfg)

	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.connecting)
	n.connecting = nil
	if n.closed {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, errNetworkClosed
	}
	if err != nil {
		return nil, fmt.Errorf("reach workspace server: %w", err)
	}
	n.conn = conn
	n.client = meshrpc.NewClient(conn)
	return n.client, nil
}

// dropIfGone forgets the connection when err says the server is not
// there to answer, so the next use starts one.
func (n *network) dropIfGone(err error) {
	if status.Code(err) != codes.Unavailable {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn != nil {
		_ = n.conn.Close()
		n.conn, n.client = nil, nil
	}
}

// startAutoJoin puts the machine on the network off the boot path when
// the user asked for it. The workspace server joins on its own when it
// starts; asking again covers a server that is already running and
// could not join the last time. Failures stay out of the UI on
// purpose: a signed-out or unentitled account simply does not join,
// and the prompt comes when the user asks for the network explicitly.
func (n *network) startAutoJoin() {
	autoJoin(n.autoJoin, n.join)
}

// autoJoin runs join in the background when enabled. A signed-out or
// unentitled account is logged as not joining rather than as a
// failure.
func autoJoin(enabled bool, join func(context.Context) error) {
	if !enabled {
		return
	}
	go debug.CapturePanicReport(func() {
		if err := join(context.Background()); err != nil {
			if errors.Is(err, runenet.ErrNotAuthenticated) ||
				errors.Is(err, runenet.ErrSubscriptionRequired) {
				log.Infof("network: not joining: %v", err)
			} else {
				log.Errorf("could not join network: %v", err)
			}
		}
	})
}

// join asks the workspace server to bring the node online, starting
// the server first if needed. It is idempotent, so a machine that could
// not join at startup joins on the first `network up` or rune:// open
// that follows. An account the plan does not cover is turned away
// before a server is started on its behalf: the server could not join
// either, and the prompt that fixes it belongs to whoever asked.
func (n *network) join(ctx context.Context) error {
	if err := n.check(ctx); err != nil {
		return err
	}
	client, err := n.connect(ctx)
	if err != nil {
		return err
	}
	err = client.Join(ctx)
	n.dropIfGone(err)
	return err
}

// joinAsync joins the mesh off the event loop. Failures are logged
// rather than reported: the caller's reconnect loop is what tells the
// user whether the peer became reachable.
func (n *network) joinAsync() {
	go debug.CapturePanicReport(func() {
		if err := n.join(context.Background()); err != nil {
			log.Warnf("could not join network: %v", err)
		}
	})
}

// downTimeout bounds taking the node offline on logout.
const downTimeout = 10 * time.Second

// down takes the machine off the network. A server that is not running
// has no node to take down.
func (n *network) down(ctx context.Context) error {
	client, err := n.running(ctx)
	if err != nil {
		return nil
	}
	err = client.Down(ctx)
	n.dropIfGone(err)
	if errors.Is(err, runenet.ErrNotStarted) {
		return nil
	}
	return err
}

// loggedIn is what a successful `login` triggers: a machine the user
// wants on the network joins with the account they just signed into.
func (n *network) loggedIn() {
	if n.autoJoin {
		n.joinAsync()
	}
}

// loggedOut is what a successful `logout` triggers. The node holds
// credentials minted for the account that just signed out, so it
// leaves the network rather than keep acting for it.
func (n *network) loggedOut(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, downTimeout)
	defer cancel()
	if err := n.down(ctx); err != nil {
		log.Warnf("network: could not leave the network on logout: %v", err)
	}
}

// loginClient wraps the account client the `login` and `logout`
// commands run on, so the network reacts to both.
func (n *network) loginClient(client loginshell.Client) loginshell.Client {
	return networkLoginClient{Client: client, n: n}
}

type networkLoginClient struct {
	loginshell.Client
	n *network
}

func (c networkLoginClient) Login(ctx context.Context) apiclient.LoginSession {
	session := c.Client.Login(ctx)
	done := make(chan error, 1)
	go debug.CapturePanicReport(func() {
		defer close(done)
		err, ok := <-session.Done
		if !ok {
			return
		}
		if err == nil {
			c.n.loggedIn()
		}
		done <- err
	})
	return apiclient.LoginSession{URL: session.URL, Done: done}
}

func (c networkLoginClient) Logout(ctx context.Context) error {
	if err := c.Client.Logout(ctx); err != nil {
		return err
	}
	c.n.loggedOut(ctx)
	return nil
}

// check reports whether this account may use the network.
func (n *network) check(ctx context.Context) error {
	if !n.gated {
		return nil
	}
	return n.gate.check(ctx)
}

func (n *network) setPrompter(prompter *networkPrompter) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.prompter = prompter
}

func (n *network) currentPrompter() *networkPrompter {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.prompter
}

// completerOption feeds mesh peers and their directories to
// `workspaceopen` completion. The completer deliberately never
// prompts — a completion runs on a keystroke, and a keystroke must not
// open a modal — so unlike the scheme it needs no IDE and is wired at
// IDE construction.
func (n *network) completerOption() ide.Option {
	return ide.WithWorkspaceOpenCompleter(
		workspacerune.Scheme, workspacerune.Completer(gatedMesh{n}))
}

// register wires the network into a live IDE: the rune:// workspace
// scheme, the `network` console command and the watch that shows what
// the workspace server reports. The first two prompt the user to sign
// in or upgrade when the plan does not cover the network, which is why
// they are registered here rather than at IDE construction — the
// prompter cannot exist before the IDE does.
func (n *network) register(
	i *ide.IDE, scheduleNextTick func(func()) bool,
) error {
	prompter := newNetworkPrompter(i, scheduleNextTick)
	n.setPrompter(prompter)
	if err := i.RegisterScheme(
		workspacerune.Scheme, n.schemeFunc(prompter)); err != nil {
		return fmt.Errorf("register %s scheme: %w", workspacerune.Scheme, err)
	}
	// The mesh round-trips run off the editor's event loop, which is
	// why `network` is a console command rather than an ex-command.
	h := networkconsole.New(networkconsole.Config{
		Network: gatedNetwork{n: n, prompter: prompter},
	})
	if err := i.RegisterREPLCommand(networkconsole.Manual(), h); err != nil {
		return fmt.Errorf("register '%s': %w", networkconsole.CommandName, err)
	}
	go debug.CapturePanicReport(n.watch)
	return nil
}

// watchRetryDelay is how long the watch waits before looking for the
// workspace server again after losing it, or finding none.
const watchRetryDelay = 5 * time.Second

// watch follows the workspace server's view of the node for as long as
// the editor runs, and shows the removal prompt when the account no
// longer lists this machine. It only listens to a server that is
// running: with `network.auto_join` off, nothing here starts one. A
// stream that ends — the server restarted, or was not there yet — is
// resubscribed after a pause.
func (n *network) watch() {
	ctx := n.watchCtx
	shown := false
	for ctx.Err() == nil {
		if events, err := n.subscribe(ctx); err == nil {
			for ev := range events {
				if !ev.MachineRemoved {
					shown = false
					continue
				}
				if shown {
					continue
				}
				shown = true
				if prompter := n.currentPrompter(); prompter != nil {
					prompter.prompt(runenet.ErrMachineRemoved)
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(watchRetryDelay):
		}
	}
}

func (n *network) subscribe(ctx context.Context) (<-chan meshrpc.Change, error) {
	client, err := n.running(ctx)
	if err != nil {
		return nil, err
	}
	events, err := client.Watch(ctx)
	n.dropIfGone(err)
	return events, err
}

// schemeFunc gates rune:// workspaces on the account's plan. The check
// reads cached claims only, because a workspace is opened on the event
// loop; a machine that has not joined yet does so in the background
// while the scheme's reconnect loop waits for it.
func (n *network) schemeFunc(prompter *networkPrompter) schemeapi.SchemeFunc {
	inner := workspacerune.New(gatedMesh{n})
	return func(
		ctx context.Context, cfg config.Config, uri workspaceapi.URI,
	) (schemeapi.Scheme, error) {
		if err := n.check(ctx); err != nil {
			prompter.prompt(err)
			return nil, err
		}
		n.joinAsync()
		return inner(ctx, cfg, uri)
	}
}

// gatedMesh hides the mesh from an account whose plan does not cover
// the network: a completion runs on a keystroke, and a keystroke must
// not open a modal. Dials go through the workspace server's mesh
// socket, starting the server when none is running.
type gatedMesh struct {
	n *network
}

func (g gatedMesh) Peers(ctx context.Context) ([]runenet.Peer, error) {
	if err := g.n.check(ctx); err != nil {
		return nil, nil
	}
	client, err := g.n.running(ctx)
	if err != nil {
		return nil, nil
	}
	peers, err := client.Peers(ctx)
	g.n.dropIfGone(err)
	return peers, err
}

func (g gatedMesh) Dial(ctx context.Context, peer string) (net.Conn, error) {
	if err := g.n.check(ctx); err != nil {
		return nil, err
	}
	if _, err := g.n.connect(ctx); err != nil {
		return nil, err
	}
	return g.n.dialer.Dial(ctx, peer)
}

func (g gatedMesh) DialAttachment(ctx context.Context, peer string) (net.Conn, error) {
	if err := g.n.check(ctx); err != nil {
		return nil, err
	}
	if _, err := g.n.connect(ctx); err != nil {
		return nil, err
	}
	return g.n.dialer.DialAttachment(ctx, peer)
}

// gatedNetwork is what the `network` command operates on: the
// subcommands that need the mesh check the plan first, so a user
// without one is told how to get it instead of being told the node is
// not running.
type gatedNetwork struct {
	n        *network
	prompter *networkPrompter
}

func (g gatedNetwork) Status(ctx context.Context) (runenet.Status, error) {
	if err := g.n.check(ctx); err != nil {
		g.prompter.prompt(err)
		return runenet.Status{}, err
	}
	client, err := g.n.connect(ctx)
	if err != nil {
		return runenet.Status{}, err
	}
	st, err := client.Status(ctx)
	g.n.dropIfGone(err)
	if err != nil {
		return runenet.Status{}, err
	}
	if g.removed(ctx, st) {
		g.prompter.prompt(runenet.ErrMachineRemoved)
	}
	return st, nil
}

// removed reports whether the account no longer lists this machine
// even though the backend still believes it is a member. That is what
// a machine removed while it was asleep looks like: it wakes holding a
// netmap the coordination server has already forgotten, so the logout
// that would have told it never arrives.
func (g gatedNetwork) removed(ctx context.Context, st runenet.Status) bool {
	if !g.n.gated || st.State != runenet.StateRunning {
		return false
	}
	machines, err := g.n.gate.machines(ctx)
	if err != nil {
		return false
	}
	return machineRemoved(st.MachineID, machines)
}

// joinWaitTimeout bounds how long `network up` waits for membership.
// The backend keeps retrying past it, so on expiry the user is pointed
// at `network status` rather than left hanging on a console command.
const joinWaitTimeout = 30 * time.Second

func (g gatedNetwork) Up(ctx context.Context) error {
	if err := g.n.check(ctx); err != nil {
		g.prompter.prompt(err)
		return err
	}
	client, err := g.n.connect(ctx)
	if err != nil {
		return err
	}
	upCtx, cancel := context.WithTimeout(ctx, joinWaitTimeout)
	defer cancel()
	// `network up` is what the removal prompt tells the user to run,
	// so it has to work for the machine that slept through its own
	// removal: that node is Running on a netmap nobody else has, and
	// only an unconditional re-registration gets it back.
	if st, err := client.Status(upCtx); err == nil && g.removed(upCtx, st) {
		if err := client.Rejoin(upCtx); err != nil {
			g.prompter.prompt(err)
			return err
		}
		return nil
	}
	err = client.Up(upCtx)
	g.n.dropIfGone(err)
	if err != nil {
		if upCtx.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("not a member after %s; joining continues "+
				"in the background — run `network status`", joinWaitTimeout)
		}
		g.prompter.prompt(err)
		return err
	}
	return nil
}

func (g gatedNetwork) Peers(ctx context.Context) ([]runenet.Peer, error) {
	client, err := g.n.connect(ctx)
	if err != nil {
		return nil, err
	}
	peers, err := client.Peers(ctx)
	g.n.dropIfGone(err)
	return peers, err
}

func (g gatedNetwork) Down(ctx context.Context) error {
	client, err := g.n.connect(ctx)
	if err != nil {
		return err
	}
	err = client.Down(ctx)
	g.n.dropIfGone(err)
	return err
}

// Machines and Remove go to the account server rather than the mesh:
// a machine kept off the network by the account's machine allowance is
// not there to be asked, and removing it is how the user makes room.
func (g gatedNetwork) Machines(
	ctx context.Context,
) ([]networkconsole.Machine, error) {
	machines, err := g.n.gate.machines(ctx)
	if err != nil {
		g.prompter.prompt(err)
		return nil, err
	}
	ret := make([]networkconsole.Machine, 0, len(machines))
	for _, m := range machines {
		ret = append(ret, networkconsole.Machine{
			Hostname:  m.Hostname,
			LastSeen:  m.LastSeen,
			Online:    m.Online,
			ServeOnly: m.ServeOnly,
		})
	}
	return ret, nil
}

func (g gatedNetwork) Remove(ctx context.Context, hostname string) error {
	if err := g.n.gate.remove(ctx, hostname); err != nil {
		g.prompter.prompt(err)
		return err
	}
	return nil
}

// MachineNames feeds `network remove` completion. It never prompts,
// unlike Machines: a completion runs on a keystroke, and a keystroke
// must not open a modal. A signed-out account simply offers nothing.
func (g gatedNetwork) MachineNames(ctx context.Context) ([]string, error) {
	machines, err := g.n.gate.machines(ctx)
	if err != nil {
		return nil, err
	}
	ret := make([]string, 0, len(machines))
	for _, m := range machines {
		ret = append(ret, m.Hostname)
	}
	return ret, nil
}

// Close drops the connection to the workspace server. The node keeps
// running there: the machine stays on the network after the editor
// quits.
func (n *network) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	conn := n.conn
	n.conn, n.client = nil, nil
	n.mu.Unlock()

	n.cancelWatch()
	if conn != nil {
		return conn.Close()
	}
	return nil
}
