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
	"os"
	"os/exec"
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
	sdkrpc "github.com/unstablebuild/rune-go-sdk/api/workspaceapi/workspacerpc"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/vtewire"
	"unstable.build/rune/internal/workspace/workspaceserver"
)

// TestHeadlessServesTheMachine pins what an operator's `rune --headless`
// is: the one server of the machine, which every Rune on it reaches for
// the files, commands and terminals of any of its workspaces.
func TestHeadlessServesTheMachine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMachine(t, "")
	server := m.headless(t)
	ws := m.workspace(t, "project")
	require.NoError(t, os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hi"), 0o644))

	// The server logs where it stands after its socket already answers.
	eventually(t, "the server to report it never joins", func() (bool, string) {
		return strings.Contains(server.stdout.String(), "not joining the Rune network"),
			server.output()
	})
	assert.Contains(t, server.stdout.String(), "serving this machine's workspaces on "+
		workspaceserver.SocketPath(m.dataDir), "an operator watches the server's log")
	assert.Contains(t, server.stdout.String(), "network.auto_join is off in "+m.config+
		": serving this machine only, not joining the Rune network",
		"an operator is told why the machine never joins")
	assert.Equal(t, server.cmd.Process.Pid, m.serverPID(t), "the server holds the lock")

	conn, err := workspaceserver.Dial(ctx, workspaceserver.SocketPath(m.dataDir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	remote := sdkrpc.NewClient(ctx, conn)

	for _, tc := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "the files of any workspace",
			run: func(t *testing.T) {
				files, err := remote.Chroot(ws)
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
					Args:    []string{filepath.Join(ws, "touched")},
					Watcher: workspaceapi.ChanProcessWatcher(exited),
				})
				require.NoError(t, err)
				select {
				case err := <-exited:
					require.NoError(t, err)
				case <-time.After(eventuallyTimeout):
					t.Fatal("the command never finished")
				}
				assert.FileExists(t, filepath.Join(ws, "touched"))
			},
		},
		{
			name: "the terminals of any workspace",
			run: func(t *testing.T) {
				// A client that may not start a server of its own can
				// only be reaching the operator's.
				c := m.passiveClient(t, ws)
				v, err := c.CreateVTE(vteCtx(), terminalOptions("/bin/sh",
					script(t, ws, "hello.sh", "echo hello-from-headless\nexec cat\n")))
				require.NoError(t, err)
				t.Cleanup(func() { _ = v.Close() })
				awaitScreen(t, v, "hello-from-headless")
				typeLine(v, "typed-by-the-client")
				awaitScreen(t, v, "typed-by-the-client")
				assertListed(t, c, v.ID())

				require.NoError(t, c.DeleteVTE(ctx, v.ID()))
				_, err = c.AttachVTE(vteCtx(), v.ID())
				assert.True(t, isNotFound(err), "a deleted terminal is gone; got %v", err)
			},
		},
	} {
		t.Run(tc.name, tc.run)
	}
	assert.Equal(t, []int{server.cmd.Process.Pid}, m.servers(t),
		"nothing started a second server")
}

// TestHeadlessIsTheOnlyServer pins the data directory's lock: whoever
// starts a second server is told who holds it, except a Rune that
// spawned one, for which a running server is all it wanted.
func TestHeadlessIsTheOnlyServer(t *testing.T) {
	t.Parallel()
	operator := func(t *testing.T, m *machine) int {
		return m.headless(t).cmd.Process.Pid
	}
	spawnedByARune := func(t *testing.T, m *machine) int {
		_, err := m.client(t, m.workspace(t, "ws")).ListVTEs(context.Background())
		require.NoError(t, err)
		return m.serverPID(t)
	}
	const refused = "there's another instance of Rune holding the lock on %s (pid %d)\n"

	for _, tc := range []struct {
		name       string
		running    func(t *testing.T, m *machine) int
		args       []string
		wantExit   int
		wantStderr string
		wantLog    string
	}{
		{
			name:       "an operator's second server",
			running:    operator,
			args:       []string{"--headless"},
			wantExit:   1,
			wantStderr: refused,
		},
		{
			name:       "a service manager's server beside one a Rune spawned",
			running:    spawnedByARune,
			args:       []string{"--headless"},
			wantExit:   1,
			wantStderr: refused,
		},
		{
			name:     "a Rune's server beside an operator's",
			running:  operator,
			args:     []string{"--headless", "--daemon"},
			wantLog:  "not starting a workspace server",
			wantExit: 0,
		},
		{
			name:     "a Rune's server beside one another Rune spawned",
			running:  spawnedByARune,
			args:     []string{"--headless", "--daemon"},
			wantLog:  "not starting a workspace server",
			wantExit: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMachine(t, "")
			pid := tc.running(t, m)

			second := m.run(t, tc.args...)
			require.Equal(t, tc.wantExit, second.exitCode(t), second.output())
			if tc.wantStderr != "" {
				assert.Equal(t, fmt.Sprintf(tc.wantStderr,
					workspaceserver.LockPath(m.dataDir), pid), second.stderr.String())
				assert.NotContains(t, second.stdout.String(), "holding the lock",
					"an operator is told once")
			} else {
				assert.Empty(t, second.stderr.String())
			}
			if tc.wantLog != "" {
				log, err := os.ReadFile(filepath.Join(m.dataDir, "server.log"))
				require.NoError(t, err)
				assert.Contains(t, string(log), tc.wantLog)
			}
			assert.Equal(t, pid, m.serverPID(t), "the running server keeps the lock")
			assert.True(t, alive(pid) && m.serving(), "the running server keeps serving")
		})
	}
}

// TestHeadlessStops pins a clean stop: asked to, the server exits
// successfully, ends the processes of its terminals and leaves nothing
// behind that keeps the next server from starting.
func TestHeadlessStops(t *testing.T) {
	t.Parallel()

	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			m := newMachine(t, "")
			server := m.headless(t)
			ws := m.workspace(t, "ws")
			pidFile := filepath.Join(ws, "pid")
			v, err := m.passiveClient(t, ws).CreateVTE(vteCtx(), terminalOptions("/bin/sh",
				script(t, ws, "pid.sh", "echo $$ > "+pidFile+"\necho started\nexec sleep 600\n")))
			require.NoError(t, err)
			t.Cleanup(func() { _ = v.Close() })
			awaitScreen(t, v, "started")
			raw, err := os.ReadFile(pidFile)
			require.NoError(t, err)
			shell, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			require.NoError(t, err)

			require.NoError(t, server.cmd.Process.Signal(sig))
			require.Equal(t, 0, server.exitCode(t), server.output())

			for _, path := range []string{
				workspaceserver.SocketPath(m.dataDir),
				workspaceserver.VTESocketPath(m.dataDir),
			} {
				assert.NoFileExists(t, path)
			}
			eventually(t, "the terminal's process to end", func() (bool, string) {
				return !alive(shell), fmt.Sprintf("pid %d is alive", shell)
			})
			next := m.headless(t)
			assert.Equal(t, next.cmd.Process.Pid, m.serverPID(t),
				"the next server takes the lock the last one released")
		})
	}
}

// TestCrashedServerIsReplaced pins what a crash costs: the terminals the
// server owned, confirmed gone so a Rune can recreate them, and nothing
// else. The next Rune that needs a server starts one on the sockets the
// dead one left, and the Runes that were connected carry on with it.
func TestCrashedServerIsReplaced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMachine(t, "")
	ws := m.workspace(t, "ws")
	connected := m.client(t, ws)
	v, err := connected.CreateVTE(vteCtx(), terminalOptions("cat"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	crashed := m.serverPID(t)

	require.NoError(t, syscall.Kill(crashed, syscall.SIGKILL))
	eventually(t, "the server to die", func() (bool, string) {
		return !alive(crashed), fmt.Sprintf("pid %d is alive", crashed)
	})
	assert.FileExists(t, workspaceserver.SocketPath(m.dataDir),
		"a killed server cannot remove its sockets")

	_, err = m.client(t, ws).AttachVTE(vteCtx(), v.ID())
	require.True(t, isNotFound(err),
		"a new server confirms the terminal is gone; got %v", err)
	assert.NotEqual(t, crashed, m.serverPID(t))

	eventually(t, "the connected Rune to reach the new server", func() (bool, string) {
		infos, err := connected.ListVTEs(ctx)
		return err == nil && len(infos) == 0, fmt.Sprint(infos, err)
	})
	w, err := connected.CreateVTE(vteCtx(), terminalOptions("/bin/sh",
		script(t, ws, "after.sh", "echo after-the-crash\nexec cat\n")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	awaitScreen(t, w, "after-the-crash")
}

// TestRunesStartOneServer pins the start race: Runes that need the
// server at once share the one that wins, the rest exit without
// lingering, and the winner keeps a log where its data lives because
// nobody reads its output.
func TestRunesStartOneServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMachine(t, "")
	ws := m.workspace(t, "ws")
	clients := make([]vtescheme.Scheme, 6)
	for i := range clients {
		clients[i] = m.client(t, ws)
	}

	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go debug.CapturePanicReport(func() {
			defer wg.Done()
			_, errs[i] = c.ListVTEs(ctx)
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "rune %d", i)
	}
	eventually(t, "the servers that lost to exit", func() (bool, string) {
		servers := m.servers(t)
		return len(servers) == 1, fmt.Sprint(servers)
	})
	assert.Equal(t, []int{m.serverPID(t)}, m.servers(t))
	eventually(t, "every server that exited to be reaped", func() (bool, string) {
		z := zombies(t)
		return len(z) == 0, strings.Join(z, "; ")
	})

	v, err := clients[0].CreateVTE(vteCtx(), terminalOptions("cat"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	for _, c := range clients[1:] {
		assertListed(t, c, v.ID())
	}
	log, err := os.ReadFile(filepath.Join(m.dataDir, "server.log"))
	require.NoError(t, err)
	assert.Contains(t, string(log), "serving this machine's workspaces on")
}

// TestTerminalsOutliveTheirViewer is why the server exists: a Rune that
// quits, or dies, leaves its terminals running, and the next one finds
// them as they were.
func TestTerminalsOutliveTheirViewer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		hang bool
	}{
		{name: "the viewer quits"},
		{name: "the viewer is killed", hang: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			m := newMachine(t, "")
			ws := m.workspace(t, "ws")
			const marker = "left-running-by-the-viewer"
			id, viewer := m.viewer(t, ws, []string{"/bin/sh",
				script(t, ws, "marker.sh", "echo "+marker+"\nexec cat\n")}, marker, tc.hang)
			if tc.hang {
				require.NoError(t, viewer.Process.Kill())
			}
			_ = viewer.Wait()

			next := m.client(t, ws)
			eventually(t, "the terminal to be left detached", func() (bool, string) {
				infos, err := next.ListVTEs(ctx)
				ok := err == nil && len(infos) == 1 && infos[0].ID == id &&
					infos[0].Attachments == 0 && !infos[0].Complete
				return ok, fmt.Sprint(infos, err)
			})
			v, err := next.AttachVTE(vteCtx(), id)
			require.NoError(t, err)
			t.Cleanup(func() { _ = v.Close() })
			awaitScreen(t, v, marker)
			typeLine(v, "still-running")
			awaitScreen(t, v, "still-running")
		})
	}
}

// TestWorkspacesKeepToTheirOwnTerminals pins that the machine's one
// server keeps every workspace to its own terminals however a Rune
// spells it: a restored session must never adopt another's terminal.
func TestWorkspacesKeepToTheirOwnTerminals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMachine(t, "")
	alpha, beta := m.workspace(t, "alpha"), m.workspace(t, "beta")
	v, err := m.client(t, alpha).CreateVTE(vteCtx(), terminalOptions("cat"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })

	for _, tc := range []struct {
		name string
		root string
		want []workspace.VTEID
	}{
		{name: "its workspace", root: alpha, want: []workspace.VTEID{v.ID()}},
		{name: "its workspace spelt another way", root: filepath.Join(beta, "..", "alpha") + "/",
			want: []workspace.VTEID{v.ID()}},
		{name: "another workspace", root: beta},
		{name: "a workspace containing it", root: filepath.Dir(alpha)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := m.client(t, tc.root)
			assertListed(t, c, tc.want...)
			if len(tc.want) > 0 {
				return
			}
			_, err := c.AttachVTE(vteCtx(), v.ID())
			assert.True(t, isNotFound(err), "got %v", err)
			assert.True(t, isNotFound(c.DeleteVTE(ctx, v.ID())))
		})
	}
	assertListed(t, m.client(t, alpha), v.ID())
}

// TestVTEBridge pins the bridge an ssh channel runs: it carries a
// client's handshake to the machine's server unchanged, starting the
// server when none is running.
func TestVTEBridge(t *testing.T) {
	t.Parallel()
	m := newMachine(t, "")
	ws := m.workspace(t, "ws")

	created, err := vtewire.Dial(m.bridge(t), &vtewire.Hello{
		Root:   ws,
		Create: vtewire.CreateOptions{CommandAndArgs: []string{"cat"}},
		Width:  80, Height: 24,
	})
	require.NoError(t, err, "the bridge starts the server it needs")
	assert.Len(t, m.servers(t), 1)
	assertListed(t, m.passiveClient(t, ws), workspace.VTEID(created.VTEID))

	for _, tc := range []struct {
		name         string
		hello        vtewire.Hello
		wantNotFound bool
	}{
		{
			name:  "attaches to a terminal of its workspace",
			hello: vtewire.Hello{Root: ws, VTEID: created.VTEID},
		},
		{
			name:         "refuses a terminal of another workspace",
			hello:        vtewire.Hello{Root: m.workspace(t, "other"), VTEID: created.VTEID},
			wantNotFound: true,
		},
		{
			name:         "reports a terminal nobody has as missing",
			hello:        vtewire.Hello{Root: ws, VTEID: string(workspace.NewVTEID())},
			wantNotFound: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ok, err := vtewire.Dial(m.bridge(t), &tc.hello)
			if tc.wantNotFound {
				var wireErr *vtewire.Error
				require.ErrorAs(t, err, &wireErr)
				assert.True(t, wireErr.NotFound(), "got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, created.VTEID, ok.VTEID)
		})
	}
}

// TestHeadlessAppliesTheConfiguredEnvironment pins gui.env: the
// terminals a server starts see the environment its user configured.
func TestHeadlessAppliesTheConfiguredEnvironment(t *testing.T) {
	t.Parallel()
	m := newMachine(t, "gui:\n  env:\n    RUNE_E2E_GUI_ENV: from-the-config\n")
	ws := m.workspace(t, "ws")
	v, err := m.client(t, ws).CreateVTE(vteCtx(), terminalOptions("/bin/sh",
		script(t, ws, "env.sh", "echo \"env=$RUNE_E2E_GUI_ENV\"\nexec cat\n")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	awaitScreen(t, v, "env=from-the-config")
}

// TestHeadlessCommandLine pins what rune refuses before it starts any
// server: a mistake is reported, never half served.
func TestHeadlessCommandLine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		args       []string
		config     string
		wantStderr string
	}{
		{
			name:       "a spawned server that is not headless",
			args:       []string{"--daemon"},
			wantStderr: "--daemon only applies to --headless",
		},
		{
			name:       "an editor flag",
			args:       []string{"--headless", "--fps"},
			wantStderr: "--headless does not take --fps",
		},
		{
			name:       "files to edit",
			args:       []string{"--headless", "main.go"},
			wantStderr: "--headless does not take file arguments",
		},
		{
			name:       "a config it cannot read",
			args:       []string{"--headless"},
			config:     "network: [\n",
			wantStderr: "load config",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMachine(t, tc.config)
			p := m.run(t, tc.args...)
			require.Equal(t, 1, p.exitCode(t), p.output())
			assert.Contains(t, p.stderr.String(), tc.wantStderr)
			assert.NoFileExists(t, workspaceserver.SocketPath(m.dataDir))
		})
	}
}

// TestServerThatCannotStart pins where a server that cannot start says
// why: an operator reads standard error, while a server that a Rune or
// an ssh transport started, whose standard error nobody reads, writes
// its log, unless its log is what it cannot open. Nothing is left
// serving either way.
func TestServerThatCannotStart(t *testing.T) {
	t.Parallel()
	// blockSockets leaves no room for the server's lock and sockets.
	blockSockets := func(t *testing.T, m *machine) []string {
		require.NoError(t, os.WriteFile(workspaceserver.SocketDir(m.dataDir), nil, 0o600))
		return nil
	}
	// unopenableLog points the server log inside a regular file.
	unopenableLog := func(_ *testing.T, m *machine) []string {
		return []string{"--workspace-server-log", filepath.Join(m.config, "server.log")}
	}

	for _, tc := range []struct {
		name       string
		args       func(t *testing.T, m *machine) []string
		mode       []string
		wantExit   int
		wantStderr string
		wantLog    string
	}{
		{
			name:       "an operator's server without room for its sockets",
			mode:       []string{"--headless"},
			args:       blockSockets,
			wantExit:   1,
			wantStderr: "create lock dir",
		},
		{
			name:     "a spawned server without room for its sockets",
			mode:     []string{"--headless", "--daemon"},
			args:     blockSockets,
			wantExit: 1,
			wantLog:  "create lock dir",
		},
		{
			name:       "a spawned server that cannot open its log",
			mode:       []string{"--headless", "--daemon"},
			args:       unopenableLog,
			wantExit:   1,
			wantStderr: "open server log",
		},
		{
			name: "an ssh workspace server that cannot open its log",
			args: func(t *testing.T, m *machine) []string {
				return append([]string{"--workspace-server", m.workspace(t, "ws")},
					unopenableLog(t, m)...)
			},
			wantExit:   1,
			wantStderr: "open server log",
		},
		{
			name: "an ssh workspace server for a workspace that does not exist",
			args: func(_ *testing.T, m *machine) []string {
				return []string{"--workspace-server", filepath.Join(m.workspaces, "missing")}
			},
			wantExit: 3,
			wantLog:  "no such file or directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMachine(t, "")
			p := m.run(t, append(tc.mode, tc.args(t, m)...)...)
			require.Equal(t, tc.wantExit, p.exitCode(t), p.output())

			if tc.wantStderr != "" {
				assert.Contains(t, p.stderr.String(), tc.wantStderr)
			} else {
				assert.Empty(t, p.stderr.String())
			}
			if tc.wantLog != "" {
				log, err := os.ReadFile(filepath.Join(m.dataDir, "server.log"))
				require.NoError(t, err)
				assert.Contains(t, string(log), tc.wantLog)
			}
			assert.False(t, m.serving(), "nothing is left serving")
		})
	}
}

func assertListed(t *testing.T, owner workspace.VTEOwner, want ...workspace.VTEID) {
	t.Helper()
	infos, err := owner.ListVTEs(context.Background())
	require.NoError(t, err)
	got := make([]workspace.VTEID, 0, len(infos))
	for _, info := range infos {
		got = append(got, info.ID)
	}
	assert.ElementsMatch(t, want, got)
}

// zombies lists the children of this process that exited and were never
// reaped. Everything the tests start is reaped as soon as it exits, and
// a zombie has lost its name, so any one is a server nobody reaped.
func zombies(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("ps", "-ax", "-o", "pid=", "-o", "ppid=",
		"-o", "stat=", "-o", "command=").Output()
	require.NoError(t, err)
	self := strconv.Itoa(os.Getpid())
	var ret []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == self && strings.HasPrefix(fields[2], "Z") {
			ret = append(ret, line)
		}
	}
	return ret
}
