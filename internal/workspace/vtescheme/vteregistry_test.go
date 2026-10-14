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
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

// TestVTERegistryObserver pins when the registry's observer is told
// and what its snapshot reports, step by step through a terminal's
// life: a process watching the server from outside learns of every
// change from the observer alone, so a missed one is a stale display.
func TestVTERegistryObserver(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	uri, err := workspaceapi.CurrentUserHostURI(os.TempDir())
	require.NoError(t, err)
	files, err := workspace.NewFileScheme(ctx, config.NopConfig(), uri)
	require.NoError(t, err)
	r, w := io.Pipe()
	scheme := &scriptedScheme{Scheme: files, pty: &scriptedPty{r: r, w: w}}

	var changes atomic.Int32
	server := NewServer(scheme, WithRegistryObserver(func() {
		changes.Add(1)
	}))
	t.Cleanup(func() { _ = server.Close() })
	root := server.root

	var created, attached workspace.VTE
	for _, step := range []struct {
		name            string
		act             func(t *testing.T)
		wantChanges     int32
		wantAttachments int
		wantComplete    bool
		wantGone        bool
	}{
		{
			name: "create",
			act: func(t *testing.T) {
				created, err = server.CreateVTE(ctx, workspace.CreateVTEOptions{
					CommandAndArgs: []string{"cat"}, Width: 20, Height: 10,
				})
				require.NoError(t, err)
			},
			wantChanges:     1,
			wantAttachments: 1,
		},
		{
			name: "attach",
			act: func(t *testing.T) {
				attached, err = server.AttachVTE(ctx, created.ID())
				require.NoError(t, err)
			},
			wantChanges:     2,
			wantAttachments: 2,
		},
		{
			name:            "detach",
			act:             func(t *testing.T) { require.NoError(t, attached.Close()) },
			wantChanges:     3,
			wantAttachments: 1,
		},
		{
			name:            "detach again is not a change",
			act:             func(t *testing.T) { require.NoError(t, attached.Close()) },
			wantChanges:     3,
			wantAttachments: 1,
		},
		{
			name: "the process exits",
			act: func(t *testing.T) {
				require.NoError(t, w.Close())
			},
			wantChanges:     4,
			wantAttachments: 1,
			wantComplete:    true,
		},
		{
			name:        "delete",
			act:         func(t *testing.T) { require.NoError(t, server.DeleteVTE(ctx, created.ID())) },
			wantChanges: 5,
			wantGone:    true,
		},
		{
			name: "delete again is not a change",
			act: func(t *testing.T) {
				assert.ErrorIs(t, server.DeleteVTE(ctx, created.ID()), workspace.ErrVTENotFound)
			},
			wantChanges: 5,
			wantGone:    true,
		},
	} {
		t.Run(step.name, func(t *testing.T) {
			step.act(t)
			// The exit is reported from the emulator's own goroutine.
			assert.Eventually(t, func() bool {
				return changes.Load() == step.wantChanges
			}, 5*time.Second, 5*time.Millisecond, "changes reported: %d", changes.Load())

			infos := server.Terminals()
			if step.wantGone {
				assert.Empty(t, infos)
				return
			}
			require.Len(t, infos[root], 1, "snapshot: %+v", infos)
			info := infos[root][0]
			assert.Equal(t, created.ID(), info.ID)
			assert.Equal(t, []string{"cat"}, info.CommandAndArgs)
			assert.Equal(t, step.wantAttachments, info.Attachments)
			assert.Equal(t, step.wantComplete, info.Complete)
		})
	}
}
