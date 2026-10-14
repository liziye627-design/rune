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
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/runenet"
	"unstable.build/rune/internal/term/gui/appmenu"
	"unstable.build/rune/internal/term/gui/docktile"
	"unstable.build/rune/internal/workspace/workspaced"
)

// TestBuildDockTile pins the tree the Dock shows while Rune runs and
// what each row does, which must be the tree the Dock tile plug-in
// shows while Rune does not run, so quitting Rune changes nothing in
// the Dock but who answers the clicks.
func TestBuildDockTile(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	proj := filepath.Join(home, "proj")
	cmd := func(title, command string, args ...string) appmenu.Command {
		return appmenu.Command{Title: title, Command: command, Args: args}
	}

	for _, tc := range []struct {
		name        string
		status      *workspaced.Status
		wantBadge   string
		wantItems   []docktile.Item
		wantActions map[int][]appmenu.Command
	}{
		{
			name:      "no server",
			wantItems: []docktile.Item{docktile.Label("No background terminals", docktile.DotGray)},
		},
		{
			name: "connected with peers and terminals",
			status: &workspaced.Status{
				Workspaces: []workspaced.WorkspaceStatus{
					{Root: "/srv/empty"},
					{Root: proj, VTEs: []workspaced.VTEStatus{
						{ID: "t1", Title: "zsh", Attachments: 1},
						{ID: "t2", CommandAndArgs: []string{"cat", "-"}},
						{ID: "t3", Complete: true},
					}},
					{Root: "/srv/other", VTEs: []workspaced.VTEStatus{{ID: "t4", Title: "make"}}},
				},
				Network: workspaced.NetworkStatus{
					State: runenet.StateRunning, Hostname: "laptop",
					Peers: []workspaced.PeerStatus{
						{Hostname: "desk", OS: "linux", Online: true},
						{Hostname: "pi", Online: false},
					},
				},
			},
			wantBadge: "2",
			wantItems: []docktile.Item{
				docktile.Label("Connected to Rune network as laptop", docktile.DotGreen),
				docktile.Submenu("Peers",
					docktile.Item{Title: "desk  linux", Dot: docktile.DotGreen, Tag: 1},
					docktile.Item{Title: "pi", Dot: docktile.DotGray, Tag: 2},
				),
				docktile.Item{Title: "Disconnect from Network", Tag: 3},
				docktile.Separator(),
				docktile.Submenu("Terminals",
					docktile.Label("~/proj", docktile.DotNone),
					docktile.Item{Title: "zsh", Tag: 4},
					docktile.Item{Title: "cat -", Tag: 5},
					docktile.Item{Title: "shell (exited)", Tag: 6},
					docktile.Separator(),
					docktile.Label("/srv/other", docktile.DotNone),
					docktile.Item{Title: "make", Tag: 7},
				),
				docktile.Item{Title: "Open Server Log", Tag: 8},
			},
			wantActions: map[int][]appmenu.Command{
				1: {cmd("Open rune://desk/", "workspaceopen", "rune://desk/")},
				2: {cmd("Open rune://pi/", "workspaceopen", "rune://pi/")},
				3: {cmd("Disconnect from Network", "console", "network", "down")},
				4: {cmd("Open "+proj, "workspaceopen", proj),
					cmd("Attach zsh", "workspaceready", "terminalattach", "t1")},
				5: {cmd("Open "+proj, "workspaceopen", proj),
					cmd("Attach cat -", "workspaceready", "terminalattach", "t2")},
				6: {cmd("Open "+proj, "workspaceopen", proj),
					cmd("Attach shell (exited)", "workspaceready", "terminalattach", "t3")},
				7: {cmd("Open /srv/other", "workspaceopen", "/srv/other"),
					cmd("Attach make", "workspaceready", "terminalattach", "t4")},
				8: {cmd("Open Server Log", "edit", "/data/server.log")},
			},
		},
		{
			name: "not connected",
			status: &workspaced.Status{
				Network: workspaced.NetworkStatus{State: runenet.StateNotRunning},
			},
			wantItems: []docktile.Item{
				docktile.Label("Not connected to Rune network", docktile.DotGray),
				docktile.Submenu("Peers", docktile.Label("No other machines", docktile.DotNone)),
				docktile.Item{Title: "Connect to Network", Tag: 1},
				docktile.Separator(),
				docktile.Submenu("Terminals", docktile.Label("No terminals", docktile.DotNone)),
				docktile.Item{Title: "Open Server Log", Tag: 2},
			},
			wantActions: map[int][]appmenu.Command{
				1: {cmd("Connect to Network", "console", "network", "up")},
				2: {cmd("Open Server Log", "edit", "/data/server.log")},
			},
		},
		{
			name: "could not join",
			status: &workspaced.Status{
				Network: workspaced.NetworkStatus{
					State: runenet.StateNotRunning, LastError: "upgrade to join",
				},
			},
			wantItems: []docktile.Item{
				docktile.Label("Rune network: upgrade to join", docktile.DotAmber),
				docktile.Submenu("Peers", docktile.Label("No other machines", docktile.DotNone)),
				docktile.Item{Title: "Connect to Network", Tag: 1},
				docktile.Separator(),
				docktile.Submenu("Terminals", docktile.Label("No terminals", docktile.DotNone)),
				docktile.Item{Title: "Open Server Log", Tag: 2},
			},
			wantActions: map[int][]appmenu.Command{
				1: {cmd("Connect to Network", "console", "network", "up")},
				2: {cmd("Open Server Log", "edit", "/data/server.log")},
			},
		},
		{
			name: "sign-in required",
			status: &workspaced.Status{
				Network: workspaced.NetworkStatus{State: "NeedsLogin", AuthURL: "https://x"},
			},
			wantItems: []docktile.Item{
				docktile.Item{Title: "Rune network: sign-in required", Dot: docktile.DotAmber, Tag: 1},
				docktile.Submenu("Peers", docktile.Label("No other machines", docktile.DotNone)),
				docktile.Item{Title: "Connect to Network", Tag: 2},
				docktile.Separator(),
				docktile.Submenu("Terminals", docktile.Label("No terminals", docktile.DotNone)),
				docktile.Item{Title: "Open Server Log", Tag: 3},
			},
			wantActions: map[int][]appmenu.Command{
				1: {cmd("Connect to Network", "console", "network", "up")},
				2: {cmd("Connect to Network", "console", "network", "up")},
				3: {cmd("Open Server Log", "edit", "/data/server.log")},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildDockTile(tc.status, "/data")
			assert.Equal(t, tc.wantBadge, got.badge)
			assert.Equal(t, tc.wantItems, got.items)
			if tc.wantActions == nil {
				tc.wantActions = map[int][]appmenu.Command{}
			}
			assert.Equal(t, tc.wantActions, got.actions)
		})
	}
}

// TestDockTileWatcher pins that the Dock tile follows the status file:
// it shows the server that is there, shows nothing once the server is
// gone or dead, and does not reinstall a menu that did not change.
func TestDockTileWatcher(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	statusPath := workspaced.StatusPath(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(statusPath), 0o700))
	write := func(status workspaced.Status) {
		t.Helper()
		status.Schema = workspaced.StatusSchema
		data, err := json.Marshal(status)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(statusPath, data, 0o600))
	}

	states := make(chan dockTileState, 16)
	w := newDockTileWatcher(dataDir, 20*time.Millisecond, func(s dockTileState) bool {
		states <- s
		return true
	})
	t.Cleanup(w.Close)
	next := func() dockTileState {
		t.Helper()
		select {
		case s := <-states:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("the Dock tile was not updated")
			return dockTileState{}
		}
	}

	assert.Equal(t, "", next().badge, "no server at start")

	write(workspaced.Status{PID: os.Getpid(), Workspaces: []workspaced.WorkspaceStatus{
		{Root: "/p", VTEs: []workspaced.VTEStatus{{ID: "a"}, {ID: "b"}}},
	}})
	w.kick()
	assert.Equal(t, "2", next().badge, "the server's terminals")

	// A dead server's status is no status; a pid no process has.
	write(workspaced.Status{PID: 1 << 30, Workspaces: []workspaced.WorkspaceStatus{
		{Root: "/p", VTEs: []workspaced.VTEStatus{{ID: "a"}}},
	}})
	assert.Equal(t, "", next().badge, "a dead server")

	write(workspaced.Status{PID: os.Getpid()})
	got := next()
	assert.Equal(t, "", got.badge)
	assert.Equal(t, docktile.Label("Not connected to Rune network", docktile.DotGray),
		got.items[0], "a live server without terminals")

	time.Sleep(100 * time.Millisecond)
	select {
	case s := <-states:
		t.Fatalf("an unchanged status was reinstalled: %+v", s)
	default:
	}

	require.NoError(t, os.Remove(statusPath))
	assert.Equal(t, docktile.Label("No background terminals", docktile.DotGray),
		next().items[0], "the server went away")
}

// A state the main thread had no room for is not on the Dock, so the
// watcher tries it again rather than waiting for the next change.
func TestDockTileWatcherRetriesADroppedState(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	applied := make(chan struct{})
	w := newDockTileWatcher(t.TempDir(), 5*time.Millisecond, func(dockTileState) bool {
		if calls.Add(1) < 3 {
			return false
		}
		close(applied)
		return true
	})
	t.Cleanup(w.Close)

	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatalf("a dropped state was never applied again (%d tries)", calls.Load())
	}
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 3, calls.Load(), "an applied state is not applied again")
}

// A server of this user is always this user's to signal, so a status
// naming a process that is not is a server that died and whose pid was
// reused, as the plug-in also concludes.
func TestDockTileStatusOfAnotherUsersProcess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may signal every process")
	}
	t.Parallel()
	assert.True(t, processAlive(os.Getpid()))
	assert.False(t, processAlive(1), "pid 1 belongs to root")
}

func TestDockTileTerminalName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		vte  workspaced.VTEStatus
		want string
	}{
		{"title", workspaced.VTEStatus{Title: "vim", CommandAndArgs: []string{"zsh"}}, "vim"},
		{"program", workspaced.VTEStatus{CommandAndArgs: []string{"cat", "-"}}, "cat -"},
		{"shell", workspaced.VTEStatus{}, "shell"},
		{"exited", workspaced.VTEStatus{Title: "make", Complete: true}, "make (exited)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, dockTileTerminalName(tc.vte))
		})
	}
}
