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

package vtetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/workspace"
	sshtest "unstable.build/rune/internal/workspace/workspacessh/test"
)

// TestRemoteTerminalSurvivesTransportLoss is the reason the workspace
// server exists: the ssh bridge is killed underneath a running terminal
// and the terminal must still be there, with its scrollback intact,
// once the scheme reconnects.
//
// Before terminals moved to the workspace server, the emulator lived in
// the `rune -x` process, so killing that process destroyed the pty, the
// shell and everything the user had on screen.
func TestRemoteTerminalSurvivesTransportLoss(t *testing.T) {
	c := startContainer(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := workspaceDir(t)
	s := newRemoteScheme(t, c, dir)

	// The marker is printed once, before the process parks: if it is
	// still on screen after the reconnect it can only have come from
	// the original emulator's grid, not from a replacement.
	const marker = "vte-survives-transport-loss"
	script := remoteScript(t, s, dir, "marker.sh",
		"echo "+marker+"\nexec sleep 600\n")
	v, err := s.CreateVTE(vteCtx(ctx), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"/bin/sh", script},
		Width:          80,
		Height:         24,
		MaxLines:       1000,
	})
	require.NoError(t, err)
	id := v.ID()
	require.NotEmpty(t, id)

	eventually(t, "the terminal to render its first output", func() (bool, string) {
		got := snapshotText(t, v)
		return strings.Contains(got, marker), fmt.Sprintf("screen=%q", got)
	})

	sshtest.TerminateRemoteRuneServer(t, c.ID)

	var reattached workspace.VTE
	eventually(t, "the terminal to be reattachable by its stable id",
		func() (bool, string) {
			got, err := s.AttachVTE(vteCtx(ctx), id)
			if err != nil {
				return false, err.Error()
			}
			reattached = got
			return true, ""
		})
	t.Cleanup(func() { _ = reattached.Close() })

	assert.Contains(t, snapshotText(t, reattached), marker,
		"the reattached terminal must be the original emulator with "+
			"its grid intact, not a fresh one")
}

// TestAttachUnknownVTEReportsNotFound pins the one condition that
// authorizes recreating a terminal from a snapshot: the server was
// reached and confirmed the emulator does not exist.
func TestAttachUnknownVTEReportsNotFound(t *testing.T) {
	c := startContainer(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s := newRemoteScheme(t, c, workspaceDir(t))

	// Create one terminal first so the workspace server is definitely
	// up: otherwise a "not found" could just as well mean "no server".
	v, err := s.CreateVTE(vteCtx(ctx), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"sleep", "600"},
		Width:          80,
		Height:         24,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })

	_, err = s.AttachVTE(vteCtx(ctx), workspace.NewVTEID())
	require.Error(t, err)
	assert.True(t, errors.Is(err, workspace.ErrVTENotFound),
		"a reached server that has no such emulator must report "+
			"ErrVTENotFound so the caller may fall back to a "+
			"snapshot; got %v", err)
}

// TestSchemesShareOneWorkspaceServer proves the socket is the election:
// two independently connected schemes on the same workspace reach the
// same server, so a terminal opened by one is visible to the other.
func TestSchemesShareOneWorkspaceServer(t *testing.T) {
	c := startContainer(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir := workspaceDir(t)
	first := newRemoteScheme(t, c, dir)
	second := newRemoteScheme(t, c, dir)

	v, err := first.CreateVTE(vteCtx(ctx), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"sleep", "600"},
		Width:          80,
		Height:         24,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	id := v.ID()

	eventually(t, "the second scheme to list the first scheme's terminal",
		func() (bool, string) {
			infos, err := second.ListVTEs(ctx)
			if err != nil {
				return false, err.Error()
			}
			for _, info := range infos {
				if info.ID == id {
					return true, ""
				}
			}
			return false, fmt.Sprintf("listed=%v", infos)
		})

	other, err := second.AttachVTE(vteCtx(ctx), id)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	assert.Equal(t, id, other.ID())
}

// TestAttachedClientsSeeTheSameLiveOutput pins the fan-out across two
// separate ssh transports: both clients replay the same byte stream in
// their own process, so output produced after both attached reaches
// both grids and the two converge on the same screen.
func TestAttachedClientsSeeTheSameLiveOutput(t *testing.T) {
	c := startContainer(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := workspaceDir(t)
	first := newRemoteScheme(t, c, dir)
	second := newRemoteScheme(t, c, dir)

	v, err := first.CreateVTE(vteCtx(ctx), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"cat"},
		Width:          80,
		Height:         24,
		MaxLines:       1000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })

	other, err := second.AttachVTE(vteCtx(ctx), v.ID())
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })

	// cat echoes the typed line back down the same stream both
	// clients read, so the marker can only appear on a grid that was
	// fed the live output.
	const marker = "typed-by-the-first-client"
	typeLine(t, v, marker)

	for name, client := range map[string]workspace.VTE{
		"typing": v, "watching": other,
	} {
		eventually(t, "the "+name+" client to render the typed line",
			func() (bool, string) {
				got := snapshotText(t, client)
				return strings.Contains(got, marker),
					fmt.Sprintf("screen=%q", got)
			})
	}

	eventually(t, "both clients to converge on the same screen",
		func() (bool, string) {
			want, got := snapshotText(t, v), snapshotText(t, other)
			return want == got, fmt.Sprintf("typing=%q watching=%q", want, got)
		})
}

// TestCloseDetachesAndDeleteTerminates separates the two ways a client
// can be done with a terminal. Closing an attachment is not a decision
// about the terminal, so the emulator stays listed and attachable; only
// DeleteVTE is destructive.
func TestCloseDetachesAndDeleteTerminates(t *testing.T) {
	c := startContainer(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s := newRemoteScheme(t, c, workspaceDir(t))

	v, err := s.CreateVTE(vteCtx(ctx), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"sleep", "600"},
		Width:          80,
		Height:         24,
	})
	require.NoError(t, err)
	id := v.ID()

	require.NoError(t, v.Close())

	again, err := s.AttachVTE(vteCtx(ctx), id)
	require.NoError(t, err,
		"closing an attachment must not terminate the terminal")
	require.NoError(t, again.Close())

	require.NoError(t, s.DeleteVTE(ctx, id))

	_, err = s.AttachVTE(vteCtx(ctx), id)
	require.Error(t, err)
	assert.True(t, errors.Is(err, workspace.ErrVTENotFound),
		"a deleted terminal must be reported as confirmed missing; got %v", err)
}
