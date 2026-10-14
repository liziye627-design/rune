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

//go:build e2e

package vtescheme

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
	"unstable.build/rune/internal/workspace/workspacetest"
)

func TestServerSchemeOwnsTerminals(t *testing.T) {
	ctx := context.Background()
	s := newTestServerScheme(t, workspacetest.ShortTempDir(t))

	v, err := s.CreateVTE(ctx, workspace.CreateVTEOptions{
		CommandAndArgs: []string{"cat"},
		Width:          20, Height: 10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, v.ID())

	// Detaching a client must leave the terminal running: only
	// DeleteVTE terminates it.
	require.NoError(t, v.Close())
	infos, err := s.ListVTEs(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Zero(t, infos[0].Attachments)
	assert.False(t, infos[0].Complete)

	again, err := s.AttachVTE(ctx, v.ID())
	require.NoError(t, err)
	assert.Equal(t, v.ID(), again.ID())

	require.NoError(t, s.DeleteVTE(ctx, v.ID()))
	_, err = s.AttachVTE(ctx, v.ID())
	assert.ErrorIs(t, err, workspace.ErrVTENotFound)
	require.Eventually(t, again.IsComplete, 5*time.Second, 10*time.Millisecond,
		"deleting a terminal must stop its process")
}

// TestServerChroot pins that one server keeps every workspace on its
// machine to its own terminals: a restored session must never adopt a
// terminal another workspace opened.
func TestServerChroot(t *testing.T) {
	ctx := context.Background()
	base := workspacetest.ShortTempDir(t)
	for _, dir := range []string{"alpha", "beta"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, dir), 0o755))
	}

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, s *Server)
	}{
		{
			name: "a view keeps owning terminals, scoped to its workspace",
			run: func(t *testing.T, s *Server) {
				alpha := chrootOwner(t, s, "alpha")
				v := createCat(t, alpha)

				assertListed(t, alpha, v.ID())
				assertListed(t, s)
				assertListed(t, chrootOwner(t, s, "beta"))
			},
		},
		{
			name: "every spelling of a workspace reaches the same terminals",
			run: func(t *testing.T, s *Server) {
				v := createCat(t, chrootOwner(t, s, "alpha"))
				for _, spelling := range []string{
					"./alpha",
					"alpha/",
					filepath.Join(base, "alpha") + "/",
					filepath.Join(base, "beta", "..", "alpha"),
				} {
					assertListed(t, chrootOwner(t, s, spelling), v.ID())
				}
			},
		},
		{
			name: "workspaces do not reach each other's terminals",
			run: func(t *testing.T, s *Server) {
				alpha := chrootOwner(t, s, "alpha")
				beta := chrootOwner(t, s, "beta")
				v := createCat(t, alpha)

				_, err := beta.AttachVTE(ctx, v.ID())
				assert.ErrorIs(t, err, workspace.ErrVTENotFound)
				assert.ErrorIs(t, beta.DeleteVTE(ctx, v.ID()), workspace.ErrVTENotFound)
				assertListed(t, alpha, v.ID())
			},
		},
		{
			name: "closing a view leaves the server and its terminals running",
			run: func(t *testing.T, s *Server) {
				view, err := s.Chroot("alpha")
				require.NoError(t, err)
				v := createCat(t, view.(workspace.VTEOwner))

				require.NoError(t, view.Close())
				assertListed(t, chrootOwner(t, s, "alpha"), v.ID())
				_, err = s.Stat(filepath.Join(base, "alpha"))
				assert.NoError(t, err, "the view shares the server's scheme")
			},
		},
		{
			name: "a missing workspace has no view",
			run: func(t *testing.T, s *Server) {
				_, err := s.Chroot("missing")
				assert.Error(t, err)
			},
		},
		{
			name: "terminals start in their workspace",
			run: func(t *testing.T, s *Server) {
				v, err := chrootOwner(t, s, "beta").CreateVTE(ctx,
					workspace.CreateVTEOptions{
						CommandAndArgs: []string{"pwd"},
						Width:          200, Height: 5,
					})
				require.NoError(t, err)
				t.Cleanup(func() { _ = v.Close() })
				awaitScreen(t, v, filepath.Join(filepath.Base(base), "beta"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newTestServerScheme(t, base))
		})
	}
}

// TestClientSchemesShareTheMachineServer pins the production shape: one
// server above every workspace, reached by a client per workspace that
// only ever sees its own terminals.
func TestClientSchemesShareTheMachineServer(t *testing.T) {
	ctx := context.Background()
	dataDir := workspacetest.ShortTempDir(t)
	base := workspacetest.ShortTempDir(t)
	server := newTestServerScheme(t, base)
	serveTerminals(t, server, dataDir)

	alpha := newTestClient(t, filepath.Join(base, "alpha"), dataDir)
	beta := newTestClient(t, filepath.Join(base, "beta"), dataDir)
	vteCtx := WithClientVTEEnv(ctx, testClientVTEEnv())

	v, err := alpha.CreateVTE(vteCtx, workspace.CreateVTEOptions{
		CommandAndArgs: []string{"cat"},
		Width:          20, Height: 10,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })

	assertListed(t, alpha, v.ID())
	assertListed(t, beta)
	assertListed(t, server)

	_, err = beta.AttachVTE(vteCtx, v.ID())
	assert.ErrorIs(t, err, workspace.ErrVTENotFound)
	assert.ErrorIs(t, beta.DeleteVTE(ctx, v.ID()), workspace.ErrVTENotFound)

	// The terminal belongs to the server, not to the client that
	// opened it.
	require.NoError(t, v.Close())
	require.NoError(t, alpha.Close())
	alpha = newTestClient(t, filepath.Join(base, "alpha"), dataDir)
	require.Eventually(t, func() bool {
		infos, err := alpha.ListVTEs(ctx)
		return err == nil && len(infos) == 1 && infos[0].Attachments == 0
	}, 5*time.Second, 10*time.Millisecond)

	again, err := alpha.AttachVTE(vteCtx, v.ID())
	require.NoError(t, err)
	t.Cleanup(func() { _ = again.Close() })
	assert.Equal(t, v.ID(), again.ID())

	require.NoError(t, alpha.DeleteVTE(ctx, v.ID()))
	_, err = alpha.AttachVTE(vteCtx, v.ID())
	assert.ErrorIs(t, err, workspace.ErrVTENotFound)
}

func TestLoopbackOwnsTerminalsInProcess(t *testing.T) {
	ctx := context.Background()
	s := NewLoopback(fileScheme(t, workspacetest.ShortTempDir(t)))
	vteCtx := WithClientVTEEnv(ctx, testClientVTEEnv())

	v, err := s.CreateVTE(vteCtx, workspace.CreateVTEOptions{
		CommandAndArgs: []string{"cat"},
		Width:          20, Height: 10,
	})
	require.NoError(t, err)
	assertListed(t, s, v.ID())

	require.NoError(t, v.Close())
	again, err := s.AttachVTE(vteCtx, v.ID())
	require.NoError(t, err)
	t.Cleanup(func() { _ = again.Close() })

	require.NoError(t, s.DeleteVTE(ctx, v.ID()))
	_, err = s.AttachVTE(vteCtx, v.ID())
	assert.ErrorIs(t, err, workspace.ErrVTENotFound)
	require.NoError(t, s.Close())
}

func TestDialAttachment(t *testing.T) {
	running := workspacetest.ShortTempDir(t)
	serveTerminals(t, newTestServerScheme(t, workspacetest.ShortTempDir(t)), running)

	for _, tc := range []struct {
		name    string
		opts    ClientOptions
		wantIs  error
		wantMsg string
	}{
		{
			name: "reaches the running server",
			opts: ClientOptions{DataDir: running},
		},
		{
			name:   "no data directory to find a server in",
			wantIs: workspace.ErrVTEUnsupported,
		},
		{
			name:    "no server running and no command to start one",
			opts:    ClientOptions{DataDir: workspacetest.ShortTempDir(t)},
			wantMsg: "no command to start one",
		},
		{
			name: "a command that cannot start a server",
			opts: ClientOptions{
				DataDir: workspacetest.ShortTempDir(t),
				Command: []string{"/nonexistent/rune", "--headless"},
			},
			wantMsg: "start workspace server",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := DialAttachment(context.Background(), tc.opts)
			if tc.wantIs != nil || tc.wantMsg != "" {
				require.Error(t, err)
				if tc.wantIs != nil {
					assert.ErrorIs(t, err, tc.wantIs)
				}
				assert.Contains(t, err.Error(), tc.wantMsg)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			// The bridge carries a client's handshake unchanged.
			_, err = vtewire.Dial(vtewire.NewConn(conn), &vtewire.Hello{
				VTEID: string(workspace.NewVTEID()),
			})
			var wireErr *vtewire.Error
			require.ErrorAs(t, err, &wireErr)
			assert.True(t, wireErr.NotFound())
		})
	}
}
