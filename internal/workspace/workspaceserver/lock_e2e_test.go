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
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helperLockEnv makes the test binary hold the lock of a data directory
// from another process, which is the only way to observe that the
// kernel, not the holder, releases it.
const helperLockEnv = "RUNE_TEST_WORKSPACE_LOCK"

// TestWorkspaceServerLockHelperProcess is re-executed as a lock holder
// by tests below; it is inert in a normal run.
func TestWorkspaceServerLockHelperProcess(t *testing.T) {
	dataDir := os.Getenv(helperLockEnv)
	if dataDir == "" {
		t.Skip("not running as the lock holder helper")
	}
	lock, err := AcquireLock(dataDir)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(3)
	}
	defer lock.Release()
	fmt.Println("locked")
	time.Sleep(helperLifetime)
}

// holdLockInProcess starts a process holding the lock of dataDir and
// returns it once the lock is held.
func holdLockInProcess(t *testing.T, dataDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run", "^TestWorkspaceServerLockHelperProcess$")
	cmd.Env = append(os.Environ(), helperLockEnv+"="+dataDir)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", line)
	return cmd
}

func TestAcquireLockAcrossProcesses(t *testing.T) {
	dataDir := t.TempDir()
	holder := holdLockInProcess(t, dataDir)

	_, err := AcquireLock(dataDir)
	require.ErrorIs(t, err, ErrLocked)
	assert.Contains(t, err.Error(), "(pid "+strconv.Itoa(holder.Process.Pid)+")",
		"the refusal names the process to look at")

	// SIGKILL gives the holder no chance to release anything: only the
	// kernel can, which is what lets a crashed server be replaced.
	require.NoError(t, holder.Process.Kill())
	_ = holder.Wait()

	lock, err := AcquireLock(dataDir)
	require.NoError(t, err)
	require.NoError(t, lock.Release())
}
