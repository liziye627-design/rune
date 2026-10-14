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

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/ide/hostenv"
	"unstable.build/rune/internal/runenet"
	"unstable.build/rune/internal/runenet/meshdial"
	"unstable.build/rune/internal/runenet/meshrpc"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspaced"
	"unstable.build/rune/internal/workspace/workspacerune"
	runetest "unstable.build/rune/internal/workspace/workspacerune/test"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// headlessHelperEnv makes the test binary run as a spawned workspace
// server: the data directory it serves and the name it joins the
// network under, separated by a colon.
const headlessHelperEnv = "RUNE_TEST_HEADLESS"

// helperLifetime bounds a spawned server nobody cleaned up.
const helperLifetime = 3 * time.Minute

// TestHeadlessHelperProcess is re-executed as `rune --headless
// --daemon` by the tests below; it is inert in a normal run. The
// coordination server and auth key arrive through the debug-build
// environment the node reads.
func TestHeadlessHelperProcess(t *testing.T) {
	spec := os.Getenv(headlessHelperEnv)
	if spec == "" {
		t.Skip("not running as a spawned workspace server")
	}
	dataDir, hostname, _ := strings.Cut(spec, ":")
	configPath := filepath.Join(dataDir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(
		"network:\n  auto_join: true\n  hostname: "+hostname+"\n"), 0o600))

	debug.DebugBuild = "true"
	*flagDataPath = dataDir
	*flagConfigPath = configPath
	*flagHeadless = true
	*flagDaemon = true

	ctx, cancel := context.WithTimeout(context.Background(), helperLifetime)
	defer cancel()
	runHeadless(ctx, hostenv.New(dataDir, ""), "", nil)
}

// TestSpawnedServerHostsTheNetwork pins the shape the network has once
// the workspace server hosts the node: a spawned server joins on its
// own, an editor on the machine reads the node through the server's
// socket and reaches peers through its mesh socket, and a terminal
// opened on a peer belongs to that peer's server, so it is still there
// once the client that opened it is gone.
func TestSpawnedServerHostsTheNetwork(t *testing.T) {
	ctx := context.Background()
	control := runetest.StartTestControl(t, true /* sameUser */)

	alpha := spawnHeadless(t, control, "alpha")
	beta := spawnHeadless(t, control, "beta")
	waitPeerSeen(t, alpha, "beta")

	dir := workspacetest.ShortTempDir(t)
	uri, err := workspaceapi.ParseURI("rune://beta" + dir)
	require.NoError(t, err)
	dialer := meshdial.Dialer{Socket: workspaceserver.NetSocketPath(alpha.dataDir)}
	scheme, err := workspacerune.New(dialer)(ctx, config.NopConfig(), uri)
	require.NoError(t, err)
	connectCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	require.NoError(t, scheme.(workspace.RemoteScheme).WaitConnected(connectCtx))

	v, err := scheme.(workspace.VTEOwner).CreateVTE(vteCtx(ctx), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"cat"}, Width: 20, Height: 10,
	})
	require.NoError(t, err)
	id := v.ID()
	require.NoError(t, v.Close())
	require.NoError(t, scheme.Close())

	// The terminal outlived its client because beta's server owns it,
	// which is what a local client of that server sees too.
	local := vtescheme.NewClient(ctx, fileScheme(t, dir), mustHostURI(t, dir),
		vtescheme.ClientOptions{DataDir: beta.dataDir})
	t.Cleanup(func() { _ = local.Close() })
	infos, err := local.ListVTEs(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, id, infos[0].ID)

	// What the Dock reads: beta's server publishes the terminal it
	// owns and the network it is on, without being asked.
	var status workspaced.Status
	require.Eventually(t, func() bool {
		var err error
		status, err = workspaced.ReadStatus(beta.dataDir)
		return err == nil && len(status.Workspaces) == 1 &&
			len(status.Workspaces[0].VTEs) == 1 &&
			status.Workspaces[0].VTEs[0].Attachments == 0 &&
			status.Network.State == runenet.StateRunning
	}, 30*time.Second, 100*time.Millisecond, "status: %+v", &status)
	assert.Equal(t, dir, status.Workspaces[0].Root)
	assert.Equal(t, string(id), status.Workspaces[0].VTEs[0].ID)
	assert.Equal(t, "beta", status.Network.Hostname)
	require.Len(t, status.Network.Peers, 1)
	assert.Equal(t, "alpha", status.Network.Peers[0].Hostname)
}

// headless is a spawned workspace server and the editor-side client
// of its node.
type headless struct {
	dataDir string
	client  *meshrpc.Client
}

// spawnHeadless starts the test binary as a spawned workspace server
// joined to control under hostname, the way an editor starts one, and
// waits until its node reports Running.
func spawnHeadless(t *testing.T, control runetest.ControlPlane, hostname string) *headless {
	t.Helper()
	dataDir := workspacetest.ShortTempDir(t)
	t.Cleanup(func() { stopHeadless(t, dataDir) })

	conn, err := workspaceserver.Connect(context.Background(), workspaceserver.Config{
		Socket: workspaceserver.SocketPath(dataDir),
		Command: func(string) (*exec.Cmd, error) {
			cmd := exec.Command(os.Args[0], "-test.run", "^TestHeadlessHelperProcess$")
			cmd.Env = append(os.Environ(),
				headlessHelperEnv+"="+dataDir+":"+hostname,
				runenet.EnvControlURL+"="+control.URL,
				runenet.EnvAuthKey+"=tskey-test",
			)
			return cmd, nil
		},
		StartTimeout: time.Minute,
	})
	require.NoError(t, err, "start workspace server %s", hostname)
	t.Cleanup(func() { _ = conn.Close() })
	h := &headless{dataDir: dataDir, client: meshrpc.NewClient(conn)}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	events, err := h.client.Watch(ctx)
	require.NoError(t, err)
	for {
		select {
		case ev, ok := <-events:
			require.True(t, ok, "watch of %s ended", hostname)
			if ev.Status.State == runenet.StateRunning && len(ev.Status.Addrs) > 0 {
				return h
			}
		case <-ctx.Done():
			st, _ := h.client.Status(context.Background())
			t.Fatalf("%s never joined: state=%q last_error=%q",
				hostname, st.State, st.LastError)
		}
	}
}

// waitPeerSeen blocks until h lists peer, so a dial does not race the
// coordination server's first netmap.
func waitPeerSeen(t *testing.T, h *headless, peer string) {
	t.Helper()
	require.Eventually(t, func() bool {
		peers, err := h.client.Peers(context.Background())
		if err != nil {
			return false
		}
		for _, p := range peers {
			if p.Hostname == peer {
				return true
			}
		}
		return false
	}, 90*time.Second, 200*time.Millisecond, "%s never saw %s", h.dataDir, peer)
}

// stopHeadless terminates the server holding the lock of dataDir.
func stopHeadless(t *testing.T, dataDir string) {
	t.Helper()
	b, err := os.ReadFile(workspaceserver.LockPath(dataDir))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid == os.Getpid() {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	require.Eventually(t, func() bool {
		lock, err := workspaceserver.AcquireLock(dataDir)
		if err != nil {
			return false
		}
		_ = lock.Release()
		return true
	}, 30*time.Second, 50*time.Millisecond)
}

func mustHostURI(t *testing.T, path string) workspaceapi.URI {
	t.Helper()
	uri, err := workspaceapi.CurrentUserHostURI(path)
	require.NoError(t, err)
	return uri
}

func fileScheme(t *testing.T, root string) schemeapi.Scheme {
	t.Helper()
	scheme, err := workspace.NewFileScheme(
		context.Background(), config.NopConfig(), mustHostURI(t, root))
	require.NoError(t, err)
	return scheme
}

func vteCtx(ctx context.Context) context.Context {
	return vtescheme.WithClientVTEEnv(ctx, vtescheme.ClientVTEEnv{
		Publisher:     workspacetest.NopVTEHost{},
		Notifications: workspacetest.NopVTEHost{},
		Tabs:          workspacetest.NopVTEHost{},
		Config:        vte.DefaultConfig(),
	})
}
