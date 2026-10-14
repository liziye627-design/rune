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
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// A terminal terminated between being found and being attached to is
// gone: the attach must not hand out an attachment to it.
func TestVTERegistryAttachToATerminatedTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	harness, _ := newScriptedServer(t)
	server := harness.scheme
	created, err := server.CreateVTE(ctx, workspace.CreateVTEOptions{Width: 20, Height: 10})
	require.NoError(t, err)
	c, ok := server.registry.lookup(server.root, created.ID())
	require.True(t, ok)

	require.NoError(t, c.destroy())
	_, err = server.AttachVTE(ctx, created.ID())
	require.ErrorIs(t, err, workspace.ErrVTENotFound)
}

// Shutdown reports a failure only when there is one: terminals that
// all terminate cleanly are not an error to a caller checking for nil.
func TestVTERegistryClose(t *testing.T) {
	t.Parallel()
	for _, terminals := range []int{0, 1, 2} {
		harness, _ := newScriptedServer(t)
		server := harness.scheme
		for range terminals {
			_, err := server.CreateVTE(context.Background(),
				workspace.CreateVTEOptions{Width: 20, Height: 10})
			require.NoError(t, err)
		}
		require.NoError(t, server.registry.Close(), "%d terminals", terminals)
	}
}

// newTestVTERegistry builds a server whose canonical emulators run
// `cat` over the local file scheme: a process that stays alive without
// a shell profile, so tests can observe detach and destroy semantics.
func newTestVTERegistry(t *testing.T) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	uri, err := workspaceapi.CurrentUserHostURI(os.TempDir())
	require.NoError(t, err)
	scheme, err := workspace.NewFileScheme(ctx, config.NopConfig(), uri)
	require.NoError(t, err)

	s := newServer(scheme, func(
		root string, opts workspace.CreateVTEOptions, _ func() (int, int),
	) (*vte.Handler, error) {
		cfg := vte.DefaultConfig()
		cfg.WidthHint = 20
		cfg.HeightHint = 10
		cfg.CommandAndArgs = opts.CommandAndArgs
		if len(cfg.CommandAndArgs) == 0 {
			cfg.CommandAndArgs = []string{"cat"}
		}
		return vte.NewHandler(workspacetest.NopVTEHost{}, workspacetest.NopVTEHost{},
			scheme, rootedExecutor{Executor: scheme, root: root},
			workspacetest.NopVTEHost{}, cfg)
	})
	t.Cleanup(func() { _ = s.Close() })
	return s
}
