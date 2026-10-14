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

package workspaced

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	sdkrpc "github.com/unstablebuild/rune-go-sdk/api/workspaceapi/workspacerpc"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// TestServerServesEveryWorkspace pins what a client of the machine's
// server reaches: the files and commands of any workspace under its
// root, and the terminals of its own workspace only.
func TestServerServesEveryWorkspace(t *testing.T) {
	ctx := context.Background()
	dataDir := workspacetest.ShortTempDir(t)
	base := workspacetest.ShortTempDir(t)
	alpha, beta := filepath.Join(base, "alpha"), filepath.Join(base, "beta")
	for _, dir := range []string{alpha, beta} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(alpha, "hello.txt"), []byte("hi"), 0o644))
	startAt(t, dataDir, base)

	conn, err := workspaceserver.Dial(ctx, workspaceserver.SocketPath(dataDir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	remote := sdkrpc.NewClient(ctx, conn)

	for _, tc := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "files of any workspace",
			run: func(t *testing.T) {
				files, err := remote.Chroot(alpha)
				require.NoError(t, err)
				info, err := files.Stat("hello.txt")
				require.NoError(t, err)
				assert.Equal(t, int64(2), info.Size())
			},
		},
		{
			name: "commands in any workspace",
			run: func(t *testing.T) {
				exited := make(chan error, 1)
				_, err := remote.StartCommand(ctx, workspaceapi.Cmd{
					Path:    "touch",
					Args:    []string{filepath.Join(beta, "touched")},
					Watcher: workspaceapi.ChanProcessWatcher(exited),
				})
				require.NoError(t, err)
				select {
				case err := <-exited:
					require.NoError(t, err)
				case <-time.After(10 * time.Second):
					t.Fatal("the command never finished")
				}
				assert.FileExists(t, filepath.Join(beta, "touched"))
			},
		},
		{
			name: "terminals of its own workspace only",
			run: func(t *testing.T) {
				alphaVTEs := newClient(t, alpha, dataDir)
				betaVTEs := newClient(t, beta, dataDir)
				v, err := alphaVTEs.CreateVTE(vteCtx(ctx), workspace.CreateVTEOptions{
					CommandAndArgs: []string{"cat"},
					Width:          20, Height: 10,
				})
				require.NoError(t, err)
				t.Cleanup(func() { _ = v.Close() })

				infos, err := alphaVTEs.ListVTEs(ctx)
				require.NoError(t, err)
				require.Len(t, infos, 1)
				assert.Equal(t, v.ID(), infos[0].ID)
				infos, err = betaVTEs.ListVTEs(ctx)
				require.NoError(t, err)
				assert.Empty(t, infos)
			},
		},
		{
			name: "terminals in the published status",
			run: func(t *testing.T) {
				v, err := newClient(t, beta, dataDir).CreateVTE(vteCtx(ctx),
					workspace.CreateVTEOptions{
						CommandAndArgs: []string{"cat"},
						Width:          20, Height: 10,
					})
				require.NoError(t, err)
				t.Cleanup(func() { _ = v.Close() })

				var status Status
				require.Eventually(t, func() bool {
					var err error
					status, err = ReadStatus(dataDir)
					return err == nil && len(status.Workspaces) == 2 &&
						status.Workspaces[1].Root == beta &&
						len(status.Workspaces[1].VTEs) == 1 &&
						status.Workspaces[1].VTEs[0].Attachments == 1
				}, 5*time.Second, 10*time.Millisecond, "status: %+v", &status)
				assert.Equal(t, alpha, status.Workspaces[0].Root)
				assert.Zero(t, status.Workspaces[0].VTEs[0].Attachments,
					"the previous client detached")
				vte := status.Workspaces[1].VTEs[0]
				assert.Equal(t, string(v.ID()), vte.ID)
				assert.Equal(t, []string{"cat"}, vte.CommandAndArgs)
				assert.False(t, vte.Complete)
			},
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestCloseReleasesEverything pins that closing the server terminates
// its terminals and gives up its sockets and lock, so the next server
// starts cleanly.
func TestCloseReleasesEverything(t *testing.T) {
	ctx := context.Background()
	dataDir := workspacetest.ShortTempDir(t)
	root := workspacetest.ShortTempDir(t)
	s, err := Start(Options{DataDir: dataDir, Scheme: newScheme(t, root)})
	require.NoError(t, err)

	v, err := newClient(t, root, dataDir).CreateVTE(vteCtx(ctx),
		workspace.CreateVTEOptions{
			CommandAndArgs: []string{"cat"},
			Width:          20, Height: 10,
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	select {
	case <-s.Done():
		t.Fatal("a serving server reports it stopped")
	default:
	}

	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "closing twice is harmless")
	<-s.Done()
	assert.NoError(t, s.Err(), "a closed server did not fail")
	for _, path := range []string{
		workspaceserver.SocketPath(dataDir),
		workspaceserver.VTESocketPath(dataDir),
		StatusPath(dataDir),
	} {
		_, err := os.Stat(path)
		assert.ErrorIs(t, err, os.ErrNotExist, "%s must be gone", path)
	}
	require.Eventually(t, v.IsComplete, 5*time.Second, 10*time.Millisecond,
		"closing the server terminates its terminals")

	start(t, dataDir)
}
