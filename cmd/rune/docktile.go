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
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/runenet"
	"unstable.build/rune/internal/term/gui/appmenu"
	"unstable.build/rune/internal/term/gui/docktile"
	"unstable.build/rune/internal/workspace/workspaced"
)

// dockTilePoll is how often the Dock tile re-reads the workspace
// server's status. Terminal changes reach the status file without
// reaching the editor's watch of the node, so polling is what catches
// them; a change to the node kicks a read right away.
const dockTilePoll = 2 * time.Second

// dockTileState is what the Dock tile shows while Rune runs: the badge,
// the menu, and what each clickable row does. It is the same tree the
// Dock tile plug-in shows while Rune is not running
// (extra/osx/RuneDockTile), so quitting Rune changes nothing in the
// Dock but who answers the clicks.
type dockTileState struct {
	badge string
	items []docktile.Item
	// actions are the commands a row dispatches, in order, by tag.
	actions map[int][]appmenu.Command
}

// buildDockTile builds the Dock tile for the status the workspace
// server of dataDir publishes; nil is no server.
func buildDockTile(status *workspaced.Status, dataDir string) dockTileState {
	b := dockTileBuilder{}
	b.actions = map[int][]appmenu.Command{}
	if status == nil {
		b.items = append(b.items, docktile.Label("No background terminals", docktile.DotGray))
		return b.state()
	}
	b.network(status.Network)
	b.items = append(b.items, docktile.Separator())
	b.terminals(status.Workspaces)
	b.items = append(b.items, b.row("Open Server Log", docktile.DotNone,
		appmenu.Command{Title: "Open Server Log", Command: "edit",
			Args: []string{filepath.Join(dataDir, "server.log")}}))

	// The badge is what runs with nobody watching: a terminal shown in
	// this Rune is not in the background, and one whose program ended
	// is not running.
	unattached := 0
	for _, ws := range status.Workspaces {
		for _, vte := range ws.VTEs {
			if vte.Attachments == 0 && !vte.Complete {
				unattached++
			}
		}
	}
	if unattached > 0 {
		b.badge = strconv.Itoa(unattached)
	}
	return b.state()
}

type dockTileBuilder struct {
	dockTileState
	nextTag int
}

func (b *dockTileBuilder) state() dockTileState {
	return b.dockTileState
}

// row is a clickable row that dispatches commands.
func (b *dockTileBuilder) row(
	title string, dot docktile.Dot, commands ...appmenu.Command,
) docktile.Item {
	// Tag 0 is AppKit's default and must not address a row.
	b.nextTag++
	b.actions[b.nextTag] = commands
	return docktile.Item{Title: title, Dot: dot, Tag: b.nextTag}
}

func (b *dockTileBuilder) network(net workspaced.NetworkStatus) {
	connect := appmenu.Command{Title: "Connect to Network",
		Command: "console", Args: []string{"network", "up"}}
	switch {
	case net.State == runenet.StateRunning:
		b.items = append(b.items, docktile.Label(
			"Connected to Rune network as "+net.Hostname, docktile.DotGreen))
	case net.State == "NeedsLogin" || net.AuthURL != "":
		// Joining again is what asks the account for the node's
		// credentials, which is the sign-in a Rune user does.
		b.items = append(b.items, b.row(
			"Rune network: sign-in required", docktile.DotAmber, connect))
	case net.State == "" || net.State == runenet.StateNotRunning || net.State == "Stopped":
		if net.LastError != "" {
			b.items = append(b.items, docktile.Label(
				"Rune network: "+net.LastError, docktile.DotAmber))
		} else {
			b.items = append(b.items, docktile.Label(
				"Not connected to Rune network", docktile.DotGray))
		}
	default:
		b.items = append(b.items, docktile.Label(
			"Rune network: "+net.State, docktile.DotAmber))
	}

	peers := make([]docktile.Item, 0, len(net.Peers))
	for _, peer := range net.Peers {
		title := peer.Hostname
		if peer.OS != "" {
			title += "  " + peer.OS
		}
		dot := docktile.DotGray
		if peer.Online {
			dot = docktile.DotGreen
		}
		uri := "rune://" + peer.Hostname + "/"
		peers = append(peers, b.row(title, dot, appmenu.Command{
			Title: "Open " + uri, Command: cmdWorkspaceOpen, Args: []string{uri},
		}))
	}
	if len(peers) == 0 {
		peers = append(peers, docktile.Label("No other machines", docktile.DotNone))
	}
	b.items = append(b.items, docktile.Submenu("Peers", peers...))

	if net.State == runenet.StateRunning {
		b.items = append(b.items, b.row("Disconnect from Network", docktile.DotNone,
			appmenu.Command{Title: "Disconnect from Network",
				Command: "console", Args: []string{"network", "down"}}))
	} else {
		b.items = append(b.items, b.row("Connect to Network", docktile.DotNone, connect))
	}
}

func (b *dockTileBuilder) terminals(workspaces []workspaced.WorkspaceStatus) {
	var items []docktile.Item
	for _, ws := range workspaces {
		if len(ws.VTEs) == 0 {
			continue
		}
		if len(items) > 0 {
			items = append(items, docktile.Separator())
		}
		items = append(items, docktile.Label(abbreviateHome(ws.Root), docktile.DotNone))
		for _, vte := range ws.VTEs {
			name := dockTileTerminalName(vte)
			// The workspace is focused, or opened, first: a terminal
			// belongs to its workspace, and only that workspace can
			// attach to it. workspaceready waits for an open to finish.
			items = append(items, b.row(name, docktile.DotNone,
				appmenu.Command{Title: "Open " + ws.Root,
					Command: cmdWorkspaceOpen, Args: []string{ws.Root}},
				appmenu.Command{Title: "Attach " + name, Command: "workspaceready",
					Args: []string{"terminalattach", vte.ID}},
			))
		}
	}
	if len(items) == 0 {
		items = append(items, docktile.Label("No terminals", docktile.DotNone))
	}
	b.items = append(b.items, docktile.Submenu("Terminals", items...))
}

// dockTileTerminalName is what the menu calls a terminal: its title,
// else its program, else "shell"; and whether its program ended.
func dockTileTerminalName(vte workspaced.VTEStatus) string {
	name := vte.Title
	if name == "" {
		name = strings.Join(vte.CommandAndArgs, " ")
	}
	if name == "" {
		name = "shell"
	}
	if vte.Complete {
		name += " (exited)"
	}
	return name
}

func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// readDockTileStatus reads what the workspace server of dataDir
// publishes. A status is only as good as the server that wrote it: a
// server killed outright leaves its status behind, so one whose
// process is gone is no status.
func readDockTileStatus(dataDir string) *workspaced.Status {
	status, err := workspaced.ReadStatus(dataDir)
	if err != nil || !processAlive(status.PID) {
		return nil
	}
	return &status
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// EPERM is a process that exists but is not ours to signal, which a
	// server of this user never is.
	return p.Signal(syscall.Signal(0)) == nil
}

// dockTileWatcher keeps the Dock tile in step with the status the
// workspace server publishes, applying a new state whenever what it
// would show changes. apply reports whether the state reached the
// Dock; one that did not is applied again at the next poll.
type dockTileWatcher struct {
	dataDir string
	poll    time.Duration
	apply   func(dockTileState) bool
	kicks   chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

func newDockTileWatcher(
	dataDir string, poll time.Duration, apply func(dockTileState) bool,
) *dockTileWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &dockTileWatcher{
		dataDir: dataDir,
		poll:    poll,
		apply:   apply,
		kicks:   make(chan struct{}, 1),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go debug.CapturePanicReport(func() { w.run(ctx) })
	return w
}

// kick asks for the status to be read now rather than at the next
// poll. It never blocks.
func (w *dockTileWatcher) kick() {
	select {
	case w.kicks <- struct{}{}:
	default:
	}
}

func (w *dockTileWatcher) run(ctx context.Context) {
	defer close(w.done)
	var last *dockTileState
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		state := buildDockTile(readDockTileStatus(w.dataDir), w.dataDir)
		if (last == nil || !reflect.DeepEqual(*last, state)) && w.apply(state) {
			last = &state
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.kicks:
		}
	}
}

func (w *dockTileWatcher) Close() {
	w.once.Do(func() {
		w.cancel()
		<-w.done
	})
}

// startDockTile has the Dock tile show the background terminals and
// the network while Rune runs. It is the running application's half of
// the Dock tile; the plug-in the app bundle ships takes over once Rune
// quits. Elsewhere than macOS the Dock tile does nothing, and nothing
// is started.
func (b *bootstrapHandler) startDockTile() {
	if !docktile.Supported {
		return
	}
	b.dockTile = newDockTileWatcher(b.dataDir, dockTilePoll, b.applyDockTile)
	if b.network != nil {
		b.network.observe(b.dockTile.kick)
	}
}

// applyDockTile installs state on the main thread, which is the only
// thread AppKit lets the Dock tile be touched from. It reports false
// when the main thread had no room for it.
func (b *bootstrapHandler) applyDockTile(state dockTileState) bool {
	return b.publishEvent(term.Event{Type: term.EventInterrupt, UserFunc: func() {
		docktile.SetBadge(state.badge)
		docktile.SetMenu(state.items, func(tag int) {
			b.activateDockTile(state.actions[tag])
		})
	}})
}

// activateDockTile runs a row's commands on the main thread, in order,
// stopping at the first that fails: the ones after it act on what it
// should have done.
func (b *bootstrapHandler) activateDockTile(commands []appmenu.Command) {
	if len(commands) == 0 {
		return
	}
	b.publishEvent(term.Event{Type: term.EventInterrupt, UserFunc: func() {
		if b.realIDE == nil {
			// The wizard is still writing the configuration these
			// commands would act on.
			_, _ = b.notifications().Notify(browserapi.LevelWarn,
				"%s is not available during setup", commands[0].Title)
			return
		}
		for _, cmd := range commands {
			if err := b.dispatchAppMenuCommand(cmd, cmd.Args...); err != nil {
				return
			}
		}
	}})
}
