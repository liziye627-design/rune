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

package workspacedtest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/workspace"
)

// TestSSHTerminalSurvivesTheConnection pins why the server belongs to
// the machine: over ssh, the `rune -x` serving a workspace dies with the
// connection, and the terminals must not.
func TestSSHTerminalSurvivesTheConnection(t *testing.T) {
	c := startContainer(t)
	t.Parallel()
	dir := home + "/ws"
	s := c.client(t, dir)
	const marker = "survives-the-connection"
	c.sh(t, fmt.Sprintf("printf 'echo %s\\nexec cat\\n' > %s/marker.sh", marker, dir))
	v, err := s.CreateVTE(vteCtx(), terminalOptions("/bin/sh", dir+"/marker.sh"))
	require.NoError(t, err)
	awaitScreen(t, v, marker)

	c.sh(t, "pkill -f '[r]une -x'")
	var again workspace.VTE
	eventually(t, "the terminal to be reattachable", func() (bool, string) {
		again, err = s.AttachVTE(vteCtx(), v.ID())
		return err == nil, fmt.Sprint(err)
	})
	t.Cleanup(func() { _ = again.Close() })
	awaitScreen(t, again, marker)
	typeLine(again, "typed-after-reconnecting")
	awaitScreen(t, again, "typed-after-reconnecting")

	servers := c.servers(t)
	require.Len(t, servers, 1)
	assert.Contains(t, servers[c.serverPID(t)], "--daemon",
		"the ssh workspace server started the machine's server")
}

// TestSSHRunsCommands pins that the `rune -x` an ssh transport runs is
// the workspace to a Rune: it runs the commands the Rune starts on the
// machine, as the ssh user.
func TestSSHRunsCommands(t *testing.T) {
	c := startContainer(t)
	t.Parallel()
	dir := home + "/ws"
	s := c.client(t, dir)

	exited := make(chan error, 1)
	_, err := s.StartCommand(context.Background(), workspaceapi.Cmd{
		Path:    "sh",
		Args:    []string{"-c", "id -un > " + dir + "/ran-by"},
		Watcher: workspaceapi.ChanProcessWatcher(exited),
	})
	require.NoError(t, err)
	eventually(t, "the command to finish", func() (bool, string) {
		select {
		case err := <-exited:
			require.NoError(t, err)
			return true, ""
		default:
			return false, "still running"
		}
	})
	assert.Equal(t, "test\n", c.sh(t, "cat "+dir+"/ran-by"))
}

// TestSSHWorkspacesShareTheMachineServer pins that every workspace of a
// remote machine reaches its one server, each seeing only its own
// terminals, and that clients on separate connections share them live.
func TestSSHWorkspacesShareTheMachineServer(t *testing.T) {
	c := startContainer(t)
	t.Parallel()
	alpha := c.client(t, home+"/alpha")
	alphaAgain := c.client(t, home+"/alpha")
	beta := c.client(t, home+"/beta")

	v, err := alpha.CreateVTE(vteCtx(), terminalOptions("cat"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	assertListed(t, alpha, v.ID())
	assertListed(t, alphaAgain, v.ID())
	assertListed(t, beta)
	_, err = beta.AttachVTE(vteCtx(), v.ID())
	assert.True(t, isNotFound(err), "got %v", err)

	watching, err := alphaAgain.AttachVTE(vteCtx(), v.ID())
	require.NoError(t, err)
	t.Cleanup(func() { _ = watching.Close() })
	typeLine(v, "typed-on-one-connection")
	awaitScreen(t, watching, "typed-on-one-connection")

	assert.Len(t, c.servers(t), 1, "one server serves every workspace")
}

// TestSSHReachesAnOperatorsServer pins that a server an operator runs —
// under a service manager, say — is the one every Rune reaches: nothing
// starts a second beside it.
func TestSSHReachesAnOperatorsServer(t *testing.T) {
	c := startContainer(t)
	t.Parallel()
	c.sh(t, "mkdir -p .rune && printf 'network:\\n  auto_join: false\\n' > .rune/config.yaml")
	c.sh(t, "nohup .local/bin/rune --headless > /tmp/headless.log 2>&1 &")
	eventually(t, "the operator's server to serve", func() (bool, string) {
		out, code := c.run(t, "test -S .rune/servers/rune.sock && cat /tmp/headless.log")
		return code == 0, out
	})
	operator := c.serverPID(t)

	v, err := c.client(t, home+"/ws").CreateVTE(vteCtx(), terminalOptions("cat"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	typeLine(v, "served-by-the-operator")
	awaitScreen(t, v, "served-by-the-operator")

	servers := c.servers(t)
	require.Len(t, servers, 1)
	assert.NotContains(t, servers[operator], "--daemon")
	assert.Contains(t, c.sh(t, "cat /tmp/headless.log"), "serving this machine's workspaces")
}

// TestSSHOperatorIsRefusedBesideASpawnedServer pins what an operator
// sees when a Rune already started the machine's server: the error
// names the lock and its holder, and the running server is untouched.
func TestSSHOperatorIsRefusedBesideASpawnedServer(t *testing.T) {
	c := startContainer(t)
	t.Parallel()
	s := c.client(t, home+"/ws")
	v, err := s.CreateVTE(vteCtx(), terminalOptions("cat"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	spawned := c.serverPID(t)

	out, code := c.run(t, ".local/bin/rune --headless 2>&1 >/dev/null")
	assert.Equal(t, 1, code)
	assert.Equal(t, fmt.Sprintf("there's another instance of Rune holding the lock "+
		"on %s/.rune/servers/rune.lock (pid %d)\n", home, spawned), out)
	assert.Equal(t, spawned, c.serverPID(t))
	assertListed(t, s, v.ID())
}

// TestSSHServerCrashCostsItsTerminals pins what a crash of the remote
// server costs: its terminals, confirmed gone so a Rune can recreate
// them, and nothing else.
func TestSSHServerCrashCostsItsTerminals(t *testing.T) {
	c := startContainer(t)
	t.Parallel()
	s := c.client(t, home+"/ws")
	v, err := s.CreateVTE(vteCtx(), terminalOptions("cat"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	crashed := c.serverPID(t)

	c.sh(t, fmt.Sprintf("kill -9 %d", crashed))
	eventually(t, "a new server to confirm the terminal is gone", func() (bool, string) {
		_, err := s.AttachVTE(vteCtx(), v.ID())
		return isNotFound(err), fmt.Sprint(err)
	})
	assert.NotEqual(t, crashed, c.serverPID(t))

	w, err := s.CreateVTE(vteCtx(), terminalOptions("cat"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	typeLine(w, "after-the-crash")
	awaitScreen(t, w, "after-the-crash")
	servers := c.servers(t)
	assert.Len(t, servers, 1, "%s", strings.Join(mapValues(servers), "; "))
}

func mapValues(m map[int]string) []string {
	ret := make([]string, 0, len(m))
	for _, v := range m {
		ret = append(ret, v)
	}
	return ret
}
