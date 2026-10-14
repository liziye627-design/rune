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

package workspaced

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspaceserver"
)

// StatusSchema is the version of [Status] a server writes. A reader
// finding a different one must not trust the rest of the file.
const StatusSchema = 1

// Status is what a running workspace server publishes about itself at
// [StatusPath], for processes that cannot or need not speak RPC to it:
// the Dock, a status bar, a shell prompt. It is a snapshot the server
// rewrites whenever something in it changes, and removes when it
// stops, so its absence means there is no server. A server that was
// killed cannot remove it: a reader must check that PID is still
// alive before trusting the rest. Its lists are never null.
type Status struct {
	Schema    int       `json:"schema"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	// Workspaces are the workspaces with terminals, ordered by root.
	Workspaces []WorkspaceStatus `json:"workspaces"`
	Network    NetworkStatus     `json:"network"`
}

// WorkspaceStatus is one workspace whose terminals the server owns.
type WorkspaceStatus struct {
	Root string      `json:"root"`
	VTEs []VTEStatus `json:"vtes"`
}

// VTEStatus is one terminal the server owns.
type VTEStatus struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// CommandAndArgs is the program the terminal runs, empty for the
	// workspace's default shell.
	CommandAndArgs []string `json:"command_and_args"`
	// Attachments is how many clients are attached; zero means the
	// terminal runs with nobody watching.
	Attachments int `json:"attachments"`
	// Complete reports that the program exited. The terminal remains
	// until it is deleted, so its final output can still be read.
	Complete  bool      `json:"complete"`
	CreatedAt time.Time `json:"created_at"`
}

// NetworkStatus is the server's membership in the Rune network.
type NetworkStatus struct {
	// State is the node's backend state: NotRunning, NeedsLogin,
	// Starting, Running, Stopped. Empty when the server hosts no node.
	State     string `json:"state"`
	Hostname  string `json:"hostname"`
	LoginName string `json:"login_name"`
	// LastError is the most recent failure to join, empty once joined.
	LastError string `json:"last_error"`
	// AuthURL is set while the node waits for an interactive login.
	AuthURL string       `json:"auth_url"`
	Peers   []PeerStatus `json:"peers"`
}

// PeerStatus is one other machine on the Rune network.
type PeerStatus struct {
	Hostname string `json:"hostname"`
	DNSName  string `json:"dns_name"`
	OS       string `json:"os"`
	Online   bool   `json:"online"`
}

// StatusPath returns where the workspace server of dataDir publishes
// its [Status].
func StatusPath(dataDir string) string {
	return filepath.Join(workspaceserver.SocketDir(dataDir), "status.json")
}

// ReadStatus reads the [Status] the workspace server of dataDir last
// published. It fails with [os.ErrNotExist] when no server is
// publishing one and refuses a status of another [StatusSchema]. The
// status of a server that was killed outlives it; see [Status].
func ReadStatus(dataDir string) (Status, error) {
	data, err := os.ReadFile(StatusPath(dataDir))
	if err != nil {
		return Status{}, err
	}
	var ret Status
	if err := json.Unmarshal(data, &ret); err != nil {
		return Status{}, fmt.Errorf("decode %s: %w", StatusPath(dataDir), err)
	}
	if ret.Schema != StatusSchema {
		return Status{}, fmt.Errorf("%s: schema %d, want %d",
			StatusPath(dataDir), ret.Schema, StatusSchema)
	}
	return ret, nil
}

// statusDebounce is how long a status writer waits after a change
// before writing, so a burst of changes — every attachment of a
// restored session — becomes one write.
const statusDebounce = 100 * time.Millisecond

// statusWriter keeps the file at path equal to what collect reports,
// rewriting it some time after each Notify and removing it on Close.
type statusWriter struct {
	path    string
	delay   time.Duration
	collect func() Status

	notify  chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func newStatusWriter(path string, delay time.Duration, collect func() Status) *statusWriter {
	w := &statusWriter{
		path:    path,
		delay:   delay,
		collect: collect,
		notify:  make(chan struct{}, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go debug.CapturePanicReport(w.run)
	return w
}

// Notify asks for the file to be rewritten. It never blocks: a
// rewrite already pending absorbs it.
func (w *statusWriter) Notify() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *statusWriter) run() {
	defer close(w.stopped)
	for {
		select {
		case <-w.stop:
			return
		case <-w.notify:
		}
		timer := time.NewTimer(w.delay)
		select {
		case <-w.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		// Whatever changed while waiting is in what is collected now.
		select {
		case <-w.notify:
		default:
		}
		if err := w.write(); err != nil {
			log.Warnf("workspaced: publish status: %v", err)
		}
	}
}

// write replaces the file atomically: a reader sees either the previous
// status or this one, never a truncated file.
func (w *statusWriter) write() error {
	data, err := json.MarshalIndent(w.collect(), "", "  ")
	if err != nil {
		return err
	}
	// The directory is the socket directory, which listening created
	// with the permissions that keep it private.
	tmp, err := os.CreateTemp(filepath.Dir(w.path), ".status-*.json")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), w.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// Close stops rewriting and removes the file: a status nobody keeps
// current must not outlive its server.
func (w *statusWriter) Close() {
	w.once.Do(func() {
		close(w.stop)
		<-w.stopped
		_ = os.Remove(w.path)
	})
}

// vteStatus converts what the terminal registry reports into what the
// status publishes, ordered by workspace root.
func vteStatus(terminals map[string][]workspace.VTEInfo) []WorkspaceStatus {
	ret := make([]WorkspaceStatus, 0, len(terminals))
	for root, infos := range terminals {
		ws := WorkspaceStatus{Root: root, VTEs: make([]VTEStatus, 0, len(infos))}
		for _, info := range infos {
			ws.VTEs = append(ws.VTEs, VTEStatus{
				ID:             string(info.ID),
				Title:          info.Title,
				CommandAndArgs: info.CommandAndArgs,
				Attachments:    info.Attachments,
				Complete:       info.Complete,
				CreatedAt:      info.CreatedAt,
			})
		}
		ret = append(ret, ws)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].Root < ret[j].Root })
	return ret
}
