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

package vtescheme

import (
	"context"
	"os"

	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/term/programstatus"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
)

// newSchemeVTEFactory returns a [VTEFactory] that hosts canonical
// emulators on scheme, driving their ptys through terminal.
//
// A workspace server has no browser: it renders into the canonical grid
// that attached clients read, so bells, notifications and tab titles
// belong to the clients rather than to the server. The emulator's
// collaborators are therefore inert here, and the grid, scrollback,
// title and process state it owns are the whole point.
func newSchemeVTEFactory(
	scheme schemeapi.Scheme, terminal schemeapi.Terminal,
) VTEFactory {
	var tempDir string
	if uri, err := scheme.URI("."); err == nil && uri.Scheme() == workspace.FileScheme {
		// The command inherits this process's environment, TMPDIR
		// included.
		tempDir = os.TempDir()
	}
	return func(
		root string, opts workspace.CreateVTEOptions,
		cellPixelSize func() (int, int),
	) (*vte.Handler, error) {
		cfg := vte.DefaultConfig()
		cfg.CommandAndArgs = opts.CommandAndArgs
		cfg.CellPixelSize = cellPixelSize
		// Only this emulator reads the file a graphics transmission
		// names; its replicas are handed what it read.
		cfg.FileSystem = scheme
		cfg.TempDir = tempDir
		if opts.Width > 0 {
			cfg.WidthHint = opts.Width
		}
		if opts.Height > 0 {
			cfg.HeightHint = opts.Height
		}
		if opts.MaxLines > 0 {
			cfg.MaxLines = opts.MaxLines
		}
		return vte.NewHandler(headlessVTEHost{}, headlessVTEHost{},
			terminal, rootedExecutor{Executor: scheme, root: root},
			headlessVTEHost{}, cfg)
	}
}

// rootedExecutor starts programs in a workspace unless told otherwise.
// One server hosts the terminals of every workspace on its machine, so
// the scheme's own root is rarely the workspace a terminal belongs to,
// and a shell opened there would start somewhere the user never asked
// for.
type rootedExecutor struct {
	schemeapi.Executor
	root string
}

func (e rootedExecutor) StartCommand(
	ctx context.Context, cmd workspaceapi.Cmd,
) (workspaceapi.Pid, error) {
	if cmd.Dir == "" {
		cmd.Dir = e.root
	}
	return e.Executor.StartCommand(ctx, cmd)
}

// headlessVTEHost stands in for the browser collaborators a canonical
// emulator has no use for on a workspace server.
type headlessVTEHost struct{}

func (headlessVTEHost) PublishEvent(term.Event) error { return nil }

func (headlessVTEHost) Notify(
	browserapi.NotificationLevel, string, ...any,
) (string, error) {
	return "", nil
}

func (headlessVTEHost) NotifyOnce(
	browserapi.NotificationLevel, string, ...any,
) (string, error) {
	return "", nil
}

func (headlessVTEHost) UpdateNotificationProgress(
	string, string, int64, int64,
) error {
	return nil
}

func (headlessVTEHost) Tab(
	workspaceapi.URI, rune, string, browserapi.Handler,
) (browserapi.Handler, error) {
	return nil, nil
}

func (headlessVTEHost) SetTabName(
	workspaceapi.URI, string, term.Attributes,
) error {
	return nil
}

func (headlessVTEHost) OnTabExit(workspaceapi.URI) bool { return false }

func (headlessVTEHost) SetTabStatus(workspaceapi.URI, programstatus.Report) error {
	return nil
}

func (headlessVTEHost) EndTabStatus(workspaceapi.URI) error { return nil }
