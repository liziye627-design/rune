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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/vtewire"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// machine is this host as one data directory sees it: the one thing its
// workspace server is keyed by.
type machine struct {
	bin     string
	dataDir string
	config  string
	// workspaces holds the workspaces the tests open.
	workspaces string
}

// newMachine prepares a data directory whose config is extra on top of
// a network the tests never join.
func newMachine(t *testing.T, extra string) *machine {
	t.Helper()
	m := &machine{
		bin:        hostRune.get(t),
		dataDir:    workspacetest.ShortTempDir(t),
		workspaces: t.TempDir(),
	}
	m.config = filepath.Join(m.dataDir, "config.yaml")
	require.NoError(t, os.WriteFile(m.config,
		[]byte("network:\n  auto_join: false\n"+extra), 0o600))
	// Servers outlive whoever started them; the machine ends with its
	// test.
	t.Cleanup(func() { m.stop(t) })
	return m
}

func (m *machine) flags() []string {
	return []string{"--datadir", m.dataDir, "--config", m.config}
}

// run starts rune with args on this machine's data directory.
func (m *machine) run(t *testing.T, args ...string) *process {
	t.Helper()
	return startProcess(t, m.bin, append(args, m.flags()...)...)
}

// headless starts the server the way an operator or a service manager
// does, and returns once it serves.
func (m *machine) headless(t *testing.T) *process {
	t.Helper()
	p := m.run(t, "--headless")
	eventually(t, "the headless server to serve", func() (bool, string) {
		if p.exited() {
			t.Fatalf("rune --headless exited with %d\n%s",
				p.cmd.ProcessState.ExitCode(), p.output())
		}
		return m.serving(), p.output()
	})
	return p
}

func (m *machine) serving() bool {
	conn, err := workspaceserver.Dial(context.Background(),
		workspaceserver.SocketPath(m.dataDir))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// workspace creates a workspace directory named name.
func (m *machine) workspace(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(m.workspaces, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

// script writes an executable shell script into dir.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755))
	return path
}

// serverCommand is how a Rune on this machine starts its server.
func (m *machine) serverCommand() []string {
	return append([]string{m.bin, "--headless", "--daemon"}, m.flags()...)
}

// client is a Rune's workspace at root, which starts the machine's
// server when none is running.
func (m *machine) client(t *testing.T, root string) vtescheme.Scheme {
	t.Helper()
	return m.newClient(t, root, m.serverCommand())
}

// passiveClient only reaches a server that is already running.
func (m *machine) passiveClient(t *testing.T, root string) vtescheme.Scheme {
	t.Helper()
	return m.newClient(t, root, nil)
}

func (m *machine) newClient(t *testing.T, root string, command []string) vtescheme.Scheme {
	t.Helper()
	uri, err := workspaceapi.CurrentUserHostURI(root)
	require.NoError(t, err)
	inner, err := workspace.NewFileScheme(context.Background(), config.NopConfig(), uri)
	require.NoError(t, err)
	client := vtescheme.NewClient(context.Background(), inner, uri,
		vtescheme.ClientOptions{DataDir: m.dataDir, Command: command})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// serverPID is the process holding the data directory's lock, which is
// the server whenever one is running.
func (m *machine) serverPID(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(workspaceserver.LockPath(m.dataDir))
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	return pid
}

// servers lists the pids of the headless runes of this data directory.
func (m *machine) servers(t *testing.T) []int {
	t.Helper()
	var pids []int
	for pid, args := range m.processes(t) {
		if strings.Contains(args, "--headless") {
			pids = append(pids, pid)
		}
	}
	return pids
}

// processes lists the runes of this data directory by pid.
func (m *machine) processes(t *testing.T) map[int]string {
	t.Helper()
	out, err := exec.Command("ps", "-ax", "-o", "pid=", "-o", "stat=", "-o", "command=").Output()
	require.NoError(t, err)
	ret := liveProcesses(string(out))
	maps.DeleteFunc(ret, func(_ int, args string) bool {
		return !strings.HasPrefix(args, m.bin) || !strings.Contains(args, m.dataDir)
	})
	return ret
}

// liveProcesses parses the `pid stat args` lines of ps into the
// arguments of each process by pid, leaving out zombies, which have
// exited and only wait to be reaped.
func liveProcesses(ps string) map[int]string {
	ret := make(map[int]string)
	for _, line := range strings.Split(ps, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[1], "Z") {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil {
			ret[pid] = strings.Join(fields[2:], " ")
		}
	}
	return ret
}

// stop asks every rune of this data directory to exit, the way a
// service manager does, and kills whatever does not.
func (m *machine) stop(t *testing.T) {
	t.Helper()
	pids := m.processes(t)
	for pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(stopTimeout)
	for pid := range pids {
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(eventuallyInterval)
		}
		if alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// alive reports whether pid is a running process, not one that exited.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// bridge is one `rune --vte-bridge` carrying one attachment over its
// standard input and output, the way an ssh channel carries it.
func (m *machine) bridge(t *testing.T) *vtewire.Conn {
	t.Helper()
	cmd := exec.Command(m.bin, append([]string{"--vte-bridge"}, m.flags()...)...)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	// Its own pipe, unlike StdoutPipe's, outlives the process, so it can
	// be reaped as soon as it exits without losing what it wrote last.
	stdout, w, err := os.Pipe()
	require.NoError(t, err)
	cmd.Stdout = w
	require.NoError(t, cmd.Start())
	_ = w.Close()
	exited := make(chan struct{})
	go debug.CapturePanicReport(func() {
		_ = cmd.Wait()
		close(exited)
	})
	t.Cleanup(func() {
		// A bridge whose client hangs up exits on its own.
		_ = stdin.Close()
		select {
		case <-exited:
		case <-time.After(stopTimeout):
			_ = cmd.Process.Kill()
			<-exited
		}
		_ = stdout.Close()
	})
	return vtewire.NewConn(stdio{Reader: stdout, WriteCloser: stdin})
}

type stdio struct {
	io.Reader
	io.WriteCloser
}

// viewerEnv makes the test binary a Rune that opens a terminal and
// quits, which is the only way to show a terminal outliving the process
// that opened it.
const viewerEnv = "RUNE_E2E_VIEWER"

type viewerSpec struct {
	Rune    string
	DataDir string
	Config  string
	Root    string
	Argv    []string
	Marker  string
	// Hang keeps the viewer running until it is killed.
	Hang bool
}

// TestViewerHelperProcess is re-executed as a viewer by the tests; it is
// inert in a normal run.
func TestViewerHelperProcess(t *testing.T) {
	raw := os.Getenv(viewerEnv)
	if raw == "" {
		t.Skip("not running as a viewer")
	}
	var spec viewerSpec
	require.NoError(t, json.Unmarshal([]byte(raw), &spec))
	m := &machine{bin: spec.Rune, dataDir: spec.DataDir, config: spec.Config}

	v, err := m.client(t, spec.Root).CreateVTE(vteCtx(), terminalOptions(spec.Argv...))
	require.NoError(t, err)
	awaitScreen(t, v, spec.Marker)
	fmt.Printf("vte=%s\n", v.ID())
	if spec.Hang {
		select {}
	}
	// Quitting detaches from the terminal, which is all a Rune that
	// quits does to it.
	require.NoError(t, v.Close())
}

// viewer runs a Rune that opens a terminal running argv in root, waits
// for marker to show, and reports the terminal before quitting, or
// before hanging until it is killed.
func (m *machine) viewer(
	t *testing.T, root string, argv []string, marker string, hang bool,
) (workspace.VTEID, *exec.Cmd) {
	t.Helper()
	spec, err := json.Marshal(viewerSpec{
		Rune: m.bin, DataDir: m.dataDir, Config: m.config,
		Root: root, Argv: argv, Marker: marker, Hang: hang,
	})
	require.NoError(t, err)
	cmd := exec.Command(os.Args[0], "-test.run", "^TestViewerHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), viewerEnv+"="+string(spec))
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	var output strings.Builder
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		output.WriteString(scanner.Text() + "\n")
		if id, ok := strings.CutPrefix(scanner.Text(), "vte="); ok {
			// Whatever else it prints must not block it.
			go debug.CapturePanicReport(func() { _, _ = io.Copy(io.Discard, stdout) })
			return workspace.VTEID(id), cmd
		}
	}
	t.Fatalf("the viewer never opened its terminal\n%s\nstderr:\n%s",
		output.String(), stderr.String())
	return "", nil
}

// isNotFound reports a terminal the server confirmed it does not have.
func isNotFound(err error) bool {
	return errors.Is(err, workspace.ErrVTENotFound)
}
