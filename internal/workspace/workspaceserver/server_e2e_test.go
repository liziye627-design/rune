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

package workspaceserver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// helperSocketEnv makes the test binary stand in for a workspace server
// so the start path can be exercised without a rune build.
const helperSocketEnv = "RUNE_TEST_WORKSPACE_SOCKET"

// helperPIDEnv makes the test binary stand in for a workspace server
// that loses the start race: it records its pid there and exits.
const helperPIDEnv = "RUNE_TEST_WORKSPACE_PID"

// helperLifetime keeps a stray helper short-lived; the test never waits
// for it, because a real workspace server outlives its client.
const helperLifetime = 10 * time.Second

// TestWorkspaceServerHelperProcess is re-executed as the workspace
// server by tests below; it is inert in a normal run.
func TestWorkspaceServerHelperProcess(t *testing.T) {
	if pidFile := os.Getenv(helperPIDEnv); pidFile != "" {
		pid := []byte(strconv.Itoa(os.Getpid()))
		if err := os.WriteFile(pidFile, pid, 0o600); err != nil {
			os.Exit(3)
		}
		return
	}
	socket := os.Getenv(helperSocketEnv)
	if socket == "" {
		t.Skip("not running as the workspace server helper")
	}
	lis, err := Listen(socket)
	if err != nil {
		// Losing the bind race is how duplicate servers are
		// prevented; exit rather than displacing the winner.
		os.Exit(3)
	}
	defer lis.Close()
	go debug.CapturePanicReport(func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	})
	time.Sleep(helperLifetime)
}

func helperCommand(t *testing.T) func(string) (*exec.Cmd, error) {
	t.Helper()
	return func(socket string) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0],
			"-test.run", "^TestWorkspaceServerHelperProcess$")
		cmd.Env = append(os.Environ(), helperSocketEnv+"="+socket)
		return cmd, nil
	}
}

func TestConnect(t *testing.T) {
	ctx := context.Background()

	t.Run("reuses a server that is already serving", func(t *testing.T) {
		socket := filepath.Join(workspacetest.ShortTempDir(t), "reuse.sock")
		lis, err := Listen(socket)
		require.NoError(t, err)
		t.Cleanup(func() { lis.Close() })

		started := false
		conn, err := Connect(ctx, Config{
			Socket: socket,
			Command: func(string) (*exec.Cmd, error) {
				started = true
				return nil, nil
			},
		})
		require.NoError(t, err)
		t.Cleanup(func() { conn.Close() })
		assert.False(t, started,
			"a running workspace server must be reused so its "+
				"terminals stay reachable")
	})

	t.Run("starts a server when none is serving", func(t *testing.T) {
		socket := filepath.Join(workspacetest.ShortTempDir(t), "start.sock")
		conn, err := Connect(ctx, Config{
			Socket:       socket,
			Command:      helperCommand(t),
			StartTimeout: 20 * time.Second,
		})
		require.NoError(t, err)
		t.Cleanup(func() { conn.Close() })

		starts := 0
		again, err := Connect(ctx, Config{
			Socket: socket,
			Command: func(socket string) (*exec.Cmd, error) {
				starts++
				return helperCommand(t)(socket)
			},
		})
		require.NoError(t, err)
		t.Cleanup(func() { again.Close() })
		assert.Zero(t, starts,
			"the second client must attach to the existing server "+
				"rather than start a duplicate")
	})

	t.Run("reports a missing server when it may not be started", func(t *testing.T) {
		socket := filepath.Join(workspacetest.ShortTempDir(t), "absent.sock")
		_, err := Connect(ctx, Config{Socket: socket})
		assert.Error(t, err)
	})
}

// TestConnectReapsTheServersItStarts pins that a server that exits — one
// that lost the start race does so at once — does not linger as a
// zombie of the client that started it, which usually outlives it.
func TestConnectReapsTheServersItStarts(t *testing.T) {
	t.Parallel()
	dir := workspacetest.ShortTempDir(t)
	pidFile := filepath.Join(dir, "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t.Cleanup(cancel)

	_, err := Connect(ctx, Config{
		Socket: filepath.Join(dir, "rune.sock"),
		Command: func(string) (*exec.Cmd, error) {
			cmd := exec.Command(os.Args[0],
				"-test.run", "^TestWorkspaceServerHelperProcess$")
			cmd.Env = append(os.Environ(), helperPIDEnv+"="+pidFile)
			return cmd, nil
		},
	})
	require.Error(t, err, "the helper never serves")

	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(raw))
	require.NoError(t, err)
	// A zombie still answers signal 0; only a reaped process is gone.
	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, 10*time.Second, 10*time.Millisecond,
		"the server exited but its client never reaped it")
}

// TestConnectFailsWhenWhatItStartedNeverServes pins that a started
// command that exits without serving is an error, not a hang.
func TestConnectFailsWhenWhatItStartedNeverServes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)

	_, err := Connect(ctx, Config{
		Socket: filepath.Join(workspacetest.ShortTempDir(t), "rune.sock"),
		Command: func(string) (*exec.Cmd, error) {
			// The test binary running no tests exits at once.
			return exec.Command(os.Args[0], "-test.run", "^$"), nil
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wait for workspace server")
}
