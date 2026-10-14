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
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkrpc "github.com/unstablebuild/rune-go-sdk/api/workspaceapi/workspacerpc"
	"unstable.build/rune/internal/workspace/workspaceserver"
)

func TestWorkspaceServerCommand(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	got := workspaceServerCommand("/data", "/data/config.yaml")
	assert.Equal(t, []string{self, "--headless", "--daemon",
		"--datadir", "/data", "--config", "/data/config.yaml"}, got)
	for _, arg := range got[1:] {
		if name, ok := strings.CutPrefix(arg, "--"); ok {
			assert.True(t, headlessFlags[name],
				"--headless would reject the --%s it is spawned with", name)
		}
	}
}

// TestStartMachineServer pins the one server per data directory: it
// serves every workspace of the machine, and a second one is refused
// with the message an operator acts on.
func TestStartMachineServer(t *testing.T) {
	dataDir, err := os.MkdirTemp("/tmp", "rune")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	ws := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hi"), 0o644))
	setFlagForTest(t, flagDataPath, dataDir)

	server, err := startMachineServer("")
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	ctx := context.Background()
	conn, err := workspaceserver.Dial(ctx, workspaceserver.SocketPath(dataDir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	files, err := sdkrpc.NewClient(ctx, conn).Chroot(ws)
	require.NoError(t, err)
	info, err := files.Stat("hello.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(2), info.Size())

	_, err = startMachineServer("")
	require.ErrorIs(t, err, workspaceserver.ErrLocked)
	assert.Contains(t, err.Error(), "there's another instance of Rune holding the lock")
}

// TestStopOnSignal pins when the workspace server an ssh transport runs
// stops: when it is asked to terminate or interrupted, and once it stops
// watching, but never on a signal it does not handle.
func TestStopOnSignal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		signal   syscall.Signal
		wantStop bool
	}{
		{name: "terminated", signal: syscall.SIGTERM, wantStop: true},
		{name: "interrupted", signal: syscall.SIGINT, wantStop: true},
		{name: "another signal", signal: syscall.SIGWINCH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := logtest.NewGlobal()
			t.Cleanup(hook.Reset)
			level := log.GetLevel()
			log.SetLevel(log.DebugLevel)
			t.Cleanup(func() { log.SetLevel(level) })
			stopped := make(chan struct{})
			awaitStop := func(why string) {
				t.Helper()
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Fatal(why)
				}
			}

			// The signal goes to the whole test binary, so this test
			// must not run in parallel with another that handles one.
			unwatch := stopOnSignal(func() { close(stopped) })
			require.NoError(t, syscall.Kill(os.Getpid(), tc.signal))
			if tc.wantStop {
				awaitStop("the server kept running")
				unwatch()
				return
			}
			require.Eventually(t, func() bool {
				for _, entry := range hook.AllEntries() {
					if strings.Contains(entry.Message, "Received unhandled signal") {
						return true
					}
				}
				return false
			}, 5*time.Second, time.Millisecond)
			select {
			case <-stopped:
				t.Fatal("the server stopped on a signal it does not handle")
			default:
			}
			unwatch()
			awaitStop("the server kept running once nothing watched for signals")
		})
	}
}
