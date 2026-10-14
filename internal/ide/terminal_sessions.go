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

package ide

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/unstablebuild/blue/iterator"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/browser"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/ide/idehistory"
	"unstable.build/rune/internal/workspace"
)

func terminalSessionURI(name string) (workspaceapi.URI, error) {
	return workspaceapi.ParseURI("terminalsession:///" + url.PathEscape(name))
}

func (e *ex) terminalInFocus() (workspace.VTE, bool) {
	content, err := e.invokeWindow().Content()
	if err != nil {
		return nil, false
	}

	switch h := content.(type) {
	case workspace.VTE:
		return h, true
	case *browser.Tab:
		if s, ok := h.Handler().(workspace.VTE); ok {
			return s, true
		}
	}

	return nil, false
}

// terminalAttachID reads the stable emulator identity a terminal
// command was invoked with.
func terminalAttachID(args []string) (workspace.VTEID, error) {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return "", errors.New("expected one terminal id")
	}
	return workspace.VTEID(strings.TrimSpace(args[0])), nil
}

// terminalattach binds this client to a terminal the workspace already
// owns. The emulator keeps one canonical grid, so attaching from a
// second client shows the same terminal rather than a copy of it.
func (e *ex) terminalattach(_ context.Context, args ...string) error {
	id, err := terminalAttachID(args)
	if err != nil {
		return err
	}
	h := newAsyncVTE(e, nil, func() (workspace.VTE, bool, error) {
		v, err := e.attachVTE(id)
		return v, false, err
	})
	if err := e.invokeWindow().SetContent(h); err != nil {
		_ = h.Close()
		return err
	}
	return nil
}

// terminaldetach releases this client's attachment to the focused
// terminal. The emulator, its pty and its process keep running in the
// workspace, so it can be attached again later or from elsewhere.
func (e *ex) terminaldetach(_ context.Context, _ ...string) error {
	if _, ok := e.terminalInFocus(); !ok {
		return errors.New("not a terminal")
	}
	e.removeTerminalWindow()
	return nil
}

// terminalclose terminates the focused terminal in the workspace that
// owns it, taking its pty and process with it. Every other client
// attached to the same emulator loses it too, which is what separates
// this from terminaldetach.
func (e *ex) terminalclose(_ context.Context, _ ...string) error {
	h, ok := e.terminalInFocus()
	if !ok {
		return errors.New("not a terminal")
	}
	if c, ok := h.(companionTerminalHandler); ok {
		h = c.vth
	}
	id := h.ID()
	if av, ok := h.(*asyncVTE); ok && id == "" {
		// Still loading: there is nothing to terminate yet.
		av.terminateOnArrival()
	}
	e.removeTerminalWindow()
	if id != "" {
		e.terminateVTE(id)
	}
	return nil
}

// removeTerminalWindow takes the terminal in focus out of its window,
// letting go of this client's attachment to it. The companion terminal
// outlives its window, so it is let go of explicitly: the next toggle
// must open a new terminal rather than show one that is gone.
func (e *ex) removeTerminalWindow() {
	win := e.invokeWindow()
	if win != e.companionTerminalWin {
		e.comp.Browser().RemoveWindowContent(win)
		return
	}
	e.companionTerminalWin.Close()
	e.companionTerminalWin = nil
	_ = e.companionTerminal.Close()
	e.companionTerminal = nil
}

// terminateVTE terminates the terminal id names without blocking the
// event loop: the workspace may be a remote one whose transport is
// slow, and the terminal is gone from this client's point of view
// either way.
func (e *ex) terminateVTE(id workspace.VTEID) {
	ws := e.workspace
	go debug.CapturePanicReport(func() {
		if err := deleteVTE(ws, id); err != nil {
			e.sched(func() {
				_, _ = e.notifications.Notify(browserapi.LevelError,
					"close terminal: %v", err)
			})
		}
	})
}

// deleteVTE terminates the terminal id names in ws. It blocks on the
// workspace. A terminal that is already gone counts as terminated.
func deleteVTE(ws workspace.VTEOwner, id workspace.VTEID) error {
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultVTESpawnTimeout)
	defer cancel()
	err := ws.DeleteVTE(ctx, id)
	if errors.Is(err, workspace.ErrVTENotFound) {
		return nil
	}
	return err
}

// attachOrRestoreTerminal reattaches the emulator a persisted session
// names, so a terminal that outlived Rune comes back live rather than
// as a picture of itself.
//
// A replacement is built only once the workspace has confirmed the
// emulator is gone. A transport failure, an auth failure or a timeout
// must surface instead: replacing on those would silently strand a
// terminal that is still running and hand the user a dead snapshot.
func (e *ex) attachOrRestoreTerminal(
	doc idehistory.TerminalSession,
) workspace.VTE {
	av := newAsyncVTE(e, doc.CommandAndArgs, func() (workspace.VTE, bool, error) {
		if doc.VTEID != "" {
			v, err := e.attachVTE(doc.VTEID)
			if err == nil {
				return v, false, nil
			}
			if !errors.Is(err, workspace.ErrVTENotFound) {
				return nil, false, fmt.Errorf(
					"attach terminal session %q: %w", doc.Name, err)
			}
		}
		v, err := e.createVTE(doc.CommandAndArgs)
		if err != nil {
			return nil, false, err
		}
		if err := v.RestoreFromSnapshot(doc.Snapshot); err != nil {
			id := v.ID()
			_ = v.Close()
			_ = deleteVTE(e.workspace, id)
			return nil, false, fmt.Errorf(
				"restore terminal session %q: %w", doc.Name, err)
		}
		return v, true, nil
	})
	av.standIn = &doc
	return av
}

// completeAttachableTerminals lists the emulators the workspace owns so
// terminalattach can be completed by identity.
func (e *ex) completeAttachableTerminals(ctx context.Context, _ textapi.Command) (
	iterator.Iterator[string], string, error,
) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	infos, err := e.workspace.ListVTEs(ctx)
	if err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, string(info.ID))
	}
	sort.Strings(ids)
	return iterator.FromSlice(ids), "", nil
}
