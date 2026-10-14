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
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// helperEnv makes the test binary run as a spawned workspace server:
// the data directory and root it serves, separated by a colon.
const helperEnv = "RUNE_TEST_WORKSPACED"

// helperLifetime bounds a spawned server nobody cleaned up.
const helperLifetime = 20 * time.Second

// TestWorkspacedHelperProcess is re-executed as a spawned workspace
// server by the tests below; it is inert in a normal run. Like `rune
// --headless --daemon`, it exits quietly when another server already
// holds the lock.
func TestWorkspacedHelperProcess(t *testing.T) {
	spec := os.Getenv(helperEnv)
	if spec == "" {
		t.Skip("not running as a spawned workspace server")
	}
	dataDir, root, _ := strings.Cut(spec, ":")
	s, err := Start(Options{DataDir: dataDir, Scheme: newScheme(t, root)})
	if errors.Is(err, workspaceserver.ErrLocked) {
		return
	}
	require.NoError(t, err)
	defer s.Close()
	select {
	case <-s.Done():
	case <-time.After(helperLifetime):
	}
}

// TestClientsStartOneServerOnDemand pins what a Rune does when no
// server is running: it starts one, and Rune processes racing to do so
// end up sharing the single server that won the lock.
func TestClientsStartOneServerOnDemand(t *testing.T) {
	ctx := context.Background()
	dataDir := workspacetest.ShortTempDir(t)
	root := workspacetest.ShortTempDir(t)
	t.Setenv(helperEnv, dataDir+":"+root)
	t.Cleanup(func() { stopServer(t, dataDir) })

	opts := vtescheme.ClientOptions{
		DataDir: dataDir,
		Command: []string{os.Args[0], "-test.run", "^TestWorkspacedHelperProcess$"},
	}
	const racers = 4
	clients := make([]vtescheme.Scheme, racers)
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range clients {
		uri, err := workspaceapi.CurrentUserHostURI(root)
		require.NoError(t, err)
		clients[i] = vtescheme.NewClient(ctx, newScheme(t, root), uri, opts)
		t.Cleanup(func() { _ = clients[i].Close() })
		wg.Add(1)
		go debug.CapturePanicReport(func() {
			defer wg.Done()
			_, errs[i] = clients[i].ListVTEs(ctx)
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "client %d", i)
	}

	v, err := clients[0].CreateVTE(vteCtx(ctx), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"cat"},
		Width:          20, Height: 10,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	for i, client := range clients {
		infos, err := client.ListVTEs(ctx)
		require.NoError(t, err)
		require.Len(t, infos, 1, "client %d reached another server", i)
		assert.Equal(t, v.ID(), infos[0].ID)
	}
}

// stopServer terminates the server holding the lock of dataDir.
func stopServer(t *testing.T, dataDir string) {
	t.Helper()
	b, err := os.ReadFile(workspaceserver.LockPath(dataDir))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid == os.Getpid() {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	require.Eventually(t, func() bool {
		lock, err := workspaceserver.AcquireLock(dataDir)
		if err != nil {
			return false
		}
		_ = lock.Release()
		return true
	}, 10*time.Second, 20*time.Millisecond)
	_ = os.RemoveAll(filepath.Join(dataDir, "servers"))
}
