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

// Package vtetest holds the end-to-end tests for terminals that outlive
// the client that opened them.
//
// The behaviour under test is only observable across process and
// transport boundaries — a terminal surviving the death of the ssh
// bridge cannot be reproduced with an in-process fake — so these tests
// run against a real sshd in a container and a real workspace server
// binary, reusing the ssh harness.
package vtetest

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspacessh"
	sshtest "unstable.build/rune/internal/workspace/workspacessh/test"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// eventuallyTimeout bounds the polls below. Every wait here spans an ssh
// round trip plus, on the reattach path, a full reconnect with its retry
// backoff, so it is deliberately generous.
const eventuallyTimeout = 60 * time.Second

const eventuallyInterval = 200 * time.Millisecond

var workspaceDirCounter atomic.Uint64

// startContainer boots one container with the workspace server stand-in
// installed as the user's ~/.local/bin/rune.
func startContainer(t *testing.T) *sshtest.Container {
	t.Helper()
	sshtest.SkipIfNoDocker(t)
	sshtest.EnsureImage(t)
	return sshtest.StartContainer(t, sshtest.SSHDScenario{
		PublicKeyFile:     "/id_ed25519.pub",
		InstallRuneBinary: true,
	})
}

func sshConfig(t *testing.T) config.Config {
	t.Helper()
	return config.MapConfig(map[string]any{
		"private_keys": []any{sshtest.PrivateKeyPath(t, "id_ed25519")},
		"timeout":      "20s",
		"insecure":     true,
	})
}

// newRemoteScheme connects an ssh workspace scheme to a fresh directory
// on the container. The returned scheme reconnects on its own, which is
// what lets a test drop the transport and then reattach.
func newRemoteScheme(
	t *testing.T, c *sshtest.Container, dir string,
) vtescheme.Scheme {
	t.Helper()

	cfg := sshConfig(t)
	uri, err := workspaceapi.ParseURI("ssh://test@" + c.HostPort + dir)
	require.NoError(t, err)
	parent, err := workspaceapi.ParseURI("ssh://test@" + c.HostPort + "/tmp")
	require.NoError(t, err)

	schemeFn := workspacessh.New(errorUI{})
	ctx := context.Background()

	mkdir, err := schemeFn(ctx, cfg, parent)
	require.NoError(t, err)
	require.NoError(t, mkdir.MkdirAll(dir, 0o755))
	require.NoError(t, mkdir.Close())

	s, err := schemeFn(ctx, cfg, uri)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	owner, ok := s.(vtescheme.Scheme)
	require.True(t, ok, "ssh scheme must own terminals")
	return owner
}

// workspaceDir returns a container path unique to the calling test so
// two tests never share a workspace server.
func workspaceDir(t *testing.T) string {
	t.Helper()
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, t.Name())
	return path.Join("/tmp", fmt.Sprintf("rune_vte_%s_%d",
		safe, workspaceDirCounter.Add(1)))
}

// snapshotText renders an attachment's active screen as text so a test
// can assert on what the terminal shows.
func snapshotText(t *testing.T, v workspace.VTE) string {
	t.Helper()
	snap, err := v.Snapshot()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, row := range snap.ActiveCells() {
		for _, cell := range row {
			if cell.Ch == 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(cell.Ch)
		}
		b.WriteRune('\n')
	}
	return b.String()
}

// remoteScript writes an executable script into the workspace and
// returns its path.
//
// A terminal's command is shell-expanded argument by argument, so a
// `sh -c "echo x; sleep 600"` one-liner is rejected outright. Putting
// the shell syntax in a file keeps it out of the argument vector.
func remoteScript(
	t *testing.T, s schemeapi.Scheme, dir, name, body string,
) string {
	t.Helper()
	p := path.Join(dir, name)
	f, err := s.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	require.NoError(t, err)
	_, err = f.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return p
}

// typeLine feeds a line of input to a terminal the way a user would,
// through the attachment's key handler.
func typeLine(t *testing.T, v workspace.VTE, line string) {
	t.Helper()
	for _, r := range line {
		v.Handle(term.Event{Type: term.EventKey, Ch: r, Raw: []byte(string(r))})
	}
	v.Handle(term.Event{Type: term.EventKey, Key: term.KeyEnter, Raw: []byte{'\r'}})
}

// eventually polls cond until it holds, reporting the last observed
// detail on failure. testify's Eventually is not used because these
// conditions need to surface what they saw, not just that they timed
// out.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(eventuallyTimeout)
	var detail string
	for time.Now().Before(deadline) {
		ok, d := cond()
		if ok {
			return
		}
		detail = d
		time.Sleep(eventuallyInterval)
	}
	t.Fatalf("timed out waiting for %s; last observation: %s", what, detail)
}

// errorUI fails any interactive prompt: these tests authenticate with a
// key, so a prompt means the scenario is not what it claims to be.
type errorUI struct{}

func (errorUI) PromptSecret(context.Context, string) (string, error) {
	return "", fmt.Errorf("unexpected secret prompt")
}

func (errorUI) PromptText(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("unexpected text prompt")
}

func (errorUI) PromptChoice(context.Context, string, []string) (int, error) {
	return 0, fmt.Errorf("unexpected choice prompt")
}

func (errorUI) Notify(workspacessh.NotificationLevel, string) {}

// vteCtx carries the collaborators a client-side terminal is built
// with. A terminal replays its byte stream in the process that opened
// it, so every caller supplies the bell, notifications and tabs it
// should reach; these tests only look at the grid.
func vteCtx(ctx context.Context) context.Context {
	return vtescheme.WithClientVTEEnv(ctx, vtescheme.ClientVTEEnv{
		Publisher:     workspacetest.NopVTEHost{},
		Notifications: workspacetest.NopVTEHost{},
		Tabs:          workspacetest.NopVTEHost{},
		Config:        vte.DefaultConfig(),
	})
}
