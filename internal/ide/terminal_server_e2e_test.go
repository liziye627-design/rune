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

package ide

import (
	"context"
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
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/ide/idepkg/pkgtrust"
	termvte "unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspaced"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// machineServerEnv makes the test binary stand in for `rune --headless`
// serving the data directory it names.
const machineServerEnv = "RUNE_TEST_IDE_MACHINE_SERVER"

// TestIDEMachineServerHelperProcess is re-executed as this machine's
// workspace server by the test below; it is inert in a normal run.
func TestIDEMachineServerHelperProcess(t *testing.T) {
	dataDir := os.Getenv(machineServerEnv)
	if dataDir == "" {
		t.Skip("not running as the machine's workspace server")
	}
	uri, err := workspaceapi.CurrentUserHostURI("/")
	require.NoError(t, err)
	scheme, err := workspace.NewFileScheme(context.Background(), config.NopConfig(), uri)
	require.NoError(t, err)
	server, err := workspaced.Start(workspaced.Options{DataDir: dataDir, Scheme: scheme})
	require.NoError(t, err)
	defer server.Close()
	// A server outlives its clients; this one also bounds its own life
	// in case its test never stops it.
	select {
	case <-server.Done():
	case <-time.After(2 * time.Minute):
	}
}

// TestE2ELocalTerminalsOutliveTheIDE pins where the IDE puts the
// terminals of a local workspace: in the machine's workspace server,
// which it starts with the command it was given, so quitting the IDE
// leaves them running for the next one.
func TestE2ELocalTerminalsOutliveTheIDE(t *testing.T) {
	dir := t.TempDir()
	// Socket paths must fit sockaddr_un, which a test's temp dir
	// already exceeds on macOS.
	dataDir, err := os.MkdirTemp("/tmp", "ide")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	t.Setenv(machineServerEnv, dataDir)
	t.Cleanup(func() { stopMachineServer(dataDir) })

	const marker = "left-running-by-the-ide"
	script := filepath.Join(dir, "marker.sh")
	require.NoError(t, os.WriteFile(script,
		[]byte("#!/bin/sh\necho "+marker+"\nexec cat\n"), 0o755))
	configPath := filepath.Join(dataDir, "rune.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
editor:
  mode: modal
command:
  key: "<c-\\\\>"
workspace:
  auto_restore: false
`), 0o666))

	mu := new(sync.Mutex)
	scheduleNextTick, drainSchedule := newTestScheduler(t, mu)
	i, err := New(dir, configPath, dataDir, pkgtrust.NewStore(dataDir, nil),
		newTestStorage(t, dataDir),
		WithLocker(mu),
		WithScheduleNextTick(scheduleNextTick),
		WithPublishEvent(func(term.Event) bool { return true }),
		WithWorkspaceServerCommand([]string{os.Args[0],
			"-test.run", "^TestIDEMachineServerHelperProcess$"}),
	)
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = i.Close()
		}
	})
	root := i.Ready()
	mu.Lock()
	root.Resize(80, 24)
	mu.Unlock()
	drainSchedule()
	i.WaitWorkspaces()

	keys, err := term.ParseKeys("<c-\\\\>terminalnew<space>" + script + "<enter>")
	require.NoError(t, err)
	for _, k := range keys {
		mu.Lock()
		root.Handle(term.Event{Type: term.EventKey, Ch: k.Ch, Mod: k.Mod, Key: k.Key})
		mu.Unlock()
		i.WaitInflight()
	}

	// A client that may not start a server only reaches the one the
	// IDE started.
	uri, err := workspaceapi.CurrentUserHostURI(dir)
	require.NoError(t, err)
	inner, err := workspace.NewFileScheme(context.Background(), config.NopConfig(), uri)
	require.NoError(t, err)
	machine := vtescheme.NewClient(context.Background(), inner, uri,
		vtescheme.ClientOptions{DataDir: dataDir})
	t.Cleanup(func() { _ = machine.Close() })
	var infos []workspace.VTEInfo
	require.Eventually(t, func() bool {
		infos, err = machine.ListVTEs(context.Background())
		return err == nil && len(infos) > 0
	}, 30*time.Second, 50*time.Millisecond,
		"the IDE's terminals must live in the machine's server")

	require.NoError(t, i.Close())
	closed = true
	// The IDE keeps a shell warm for the next terminal; nobody opened
	// that one, so quitting reclaims it rather than leaving it behind.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		infos, err = machine.ListVTEs(context.Background())
		require.NoError(c, err)
		require.Len(c, infos, 1,
			"quitting the IDE leaves exactly the terminal it opened")
		assert.Equal(c, []string{script}, infos[0].CommandAndArgs)
		assert.Zero(c, infos[0].Attachments, "and leaves it detached")
		assert.False(c, infos[0].Complete, "and leaves it running")
	}, 30*time.Second, 50*time.Millisecond)

	env := vtescheme.ClientVTEEnv{
		Publisher:     workspacetest.NopVTEHost{},
		Notifications: workspacetest.NopVTEHost{},
		Tabs:          workspacetest.NopVTEHost{},
		Config:        termvte.DefaultConfig(),
	}
	v, err := machine.AttachVTE(vtescheme.WithClientVTEEnv(context.Background(), env), infos[0].ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	assert.Eventually(t, func() bool {
		snap, err := v.Snapshot()
		return err == nil && strings.Contains(term.CellsToString(snap.ActiveCells()), marker)
	}, 10*time.Second, 50*time.Millisecond, "the terminal kept what it showed")
}

// stopMachineServer ends the server a test started through the IDE:
// nothing else will, since it outlives every client by design.
func stopMachineServer(dataDir string) {
	raw, err := os.ReadFile(workspaceserver.LockPath(dataDir))
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
