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

package vtescheme

import (
	"context"
	"encoding/base64"
	"net"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/clipboard"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
)

// TestClientVTERendersCanonicalStream pins the whole point of the
// client: it runs a real emulator over the server's byte stream rather
// than drawing a copy of the server's grid.
func TestClientVTERendersCanonicalStream(t *testing.T) {
	h := newWireHarness(t)
	v := h.attachClient(t, &vtewire.Hello{Width: 40, Height: 12})
	require.NotEmpty(t, v.ID())
	assert.NotEmpty(t, v.URI().String())

	handler, ok := v.(*clientVTE)
	require.True(t, ok)
	require.NoError(t, handler.Handler.Component().WriteToPty(
		[]byte("replicated line\n")))
	awaitScreen(t, v, "replicated line")
}

// TestClientVTEReplicasConverge pins that two clients watching the
// same stream reach the same screen: they interpret it independently.
func TestClientVTEReplicasConverge(t *testing.T) {
	h := newWireHarness(t)
	first := h.attachClient(t, &vtewire.Hello{Width: 40, Height: 12})
	second := h.attachClient(t, &vtewire.Hello{
		VTEID: string(first.ID()), Width: 40, Height: 12,
	})
	assert.Equal(t, first.ID(), second.ID())

	require.NoError(t, second.(*clientVTE).Handler.Component().WriteToPty(
		[]byte("shared by both\n")))
	awaitScreen(t, first, "shared by both")
	awaitScreen(t, second, "shared by both")
}

// A replica reports the status its process exited with on the server,
// whether it was attached when the process exited or only afterwards.
func TestClientVTEReportsTheProcessExitStatus(t *testing.T) {
	for _, tc := range []struct {
		desc    string
		cmd     []string
		wantErr string
	}{
		{desc: "failure", cmd: []string{"false"}, wantErr: "exit status 1"},
		{desc: "clean exit", cmd: []string{"true"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			h := newWireHarness(t)
			first := h.attachClient(t, &vtewire.Hello{
				Create: vtewire.CreateOptions{CommandAndArgs: tc.cmd},
				Width:  20, Height: 10,
			})
			require.Eventually(t, first.IsComplete, 5*time.Second, 10*time.Millisecond)
			late := h.attachClient(t, &vtewire.Hello{
				VTEID: string(first.ID()), Width: 20, Height: 10,
			})
			require.Eventually(t, late.IsComplete, 5*time.Second, 10*time.Millisecond)

			for _, v := range []workspace.VTE{first, late} {
				if tc.wantErr == "" {
					assert.NoError(t, v.ExitErr())
				} else {
					assert.EqualError(t, v.ExitErr(), tc.wantErr)
				}
			}
		})
	}
}

// TestClientVTEConvergesAfterResize pins that a replica reflows to the
// same grid the terminal it replicates did: a resize is ordered
// against the output around it, so both interpret the same bytes at
// the same size.
func TestClientVTEConvergesAfterResize(t *testing.T) {
	h := newWireHarness(t)
	v := h.attachClient(t, &vtewire.Hello{Width: 80, Height: 24})
	client := v.(*clientVTE)

	require.NoError(t, client.Handler.Component().WriteToPty(
		[]byte("before resize\n")))
	awaitScreen(t, v, "before resize")

	v.Resize(28, 5)
	require.NoError(t, client.Handler.Component().WriteToPty(
		[]byte("after resize\n")))
	awaitScreen(t, v, "after resize")

	infos, err := h.scheme.ListVTEs(context.Background())
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, 28, infos[0].Width)
	assert.Equal(t, 5, infos[0].Height)

	canonical, err := h.scheme.AttachVTE(context.Background(), v.ID())
	require.NoError(t, err)
	t.Cleanup(func() { _ = canonical.Close() })

	require.Eventually(t, func() bool {
		want, err := canonical.Snapshot()
		require.NoError(t, err)
		got, err := v.Snapshot()
		require.NoError(t, err)
		return screenText(want.ActiveCells()) == screenText(got.ActiveCells())
	}, 10*time.Second, 5*time.Millisecond, "replica never matched the terminal")
}

// TestClientVTEReportsTitleToItsOwnUI pins what running the emulator
// locally buys: a terminal's dynamic title reaches the tabs of the
// client watching it, not those of the server that owns the pty.
func TestClientVTEReportsTitleToItsOwnUI(t *testing.T) {
	h := newWireHarness(t)
	tabs := new(recordingTabs)
	env := testClientVTEEnv()
	env.Tabs = tabs
	env.Config.DynamicTabName = true
	v := h.attachClientWith(t, &vtewire.Hello{Width: 40, Height: 12}, env)

	const title = "replicated-title"
	// cat only copies its input once a line is complete, and only that
	// copy reaches the emulator unmangled: the tty's own echo renders
	// control bytes printably.
	require.NoError(t, v.(*clientVTE).Handler.Component().WriteToPty(
		[]byte("\x1b]0;"+title+"\x07\n")))

	require.Eventually(t, func() bool {
		return v.Title() == title
	}, 10*time.Second, 5*time.Millisecond,
		"the replica never picked up the terminal's title")

	name, ok := tabs.last()
	require.True(t, ok, "the client's tabs were never told the new title")
	assert.Equal(t, title, name.name)
	assert.Equal(t, v.URI().String(), name.uri.String())
}

// TestClientVTEIsNotASecondResponder pins the single-responder rule
// over the whole stack: replicas watch the same byte stream as the
// terminal that owns the pty, so a program that asks the terminal a
// question must read exactly one answer no matter how many clients are
// attached.
func TestClientVTEIsNotASecondResponder(t *testing.T) {
	h := newWireHarness(t)
	first := h.attachClient(t, &vtewire.Hello{Width: 40, Height: 12})
	second := h.attachClient(t, &vtewire.Hello{
		VTEID: string(first.ID()), Width: 40, Height: 12,
	})
	require.Equal(t, first.ID(), second.ID())

	// A tap reads the same stream the replicas do, so it counts the
	// answers the program would read back.
	tap := h.dial(t, &vtewire.Hello{
		VTEID: string(first.ID()), Width: 40, Height: 12,
	})
	tap.readSnapshot(t)

	// cat copies the completed line back verbatim, so the emulator
	// that owns the pty sees a real query and answers it; the answer
	// travels back through the same stream every replica reads.
	require.NoError(t, first.(*clientVTE).Handler.Component().WriteToPty(
		[]byte("\x1b[6n\n")))

	// The tty renders control bytes printably when it echoes, so match
	// the report's shape rather than its escape prefix.
	cpr := regexp.MustCompile(`\[\d+;\d+R`)
	seen := tap.awaitOutputMatch(t, cpr)
	// Any second responder's answer travels the same way, so a short
	// settle is enough for it to show up.
	seen += tap.drainOutput(t, 500*time.Millisecond)
	assert.Len(t, cpr.FindAllString(seen, -1), 1,
		"exactly one emulator may answer a terminal query")
}

// TestClientVTEDeliversOSCToEveryClient pins the other half of running
// the emulator locally: an OSC the terminal emits acts on every
// attached client's own collaborators, so two Runes watching one
// terminal each get the copied text in their own clipboard.
func TestClientVTEDeliversOSCToEveryClient(t *testing.T) {
	h := newWireHarness(t)
	firstEnv, secondEnv := testClientVTEEnv(), testClientVTEEnv()
	firstEnv.Config.Clipboard = clipboard.NewInMemory()
	secondEnv.Config.Clipboard = clipboard.NewInMemory()

	first := h.attachClientWith(t,
		&vtewire.Hello{Width: 40, Height: 12}, firstEnv)
	second := h.attachClientWith(t, &vtewire.Hello{
		VTEID: string(first.ID()), Width: 40, Height: 12,
	}, secondEnv)

	const copied = "copied by osc"
	payload := base64.StdEncoding.EncodeToString([]byte(copied))
	require.NoError(t, second.(*clientVTE).Handler.Component().WriteToPty(
		[]byte("\x1b]52;c;"+payload+"\x07\n")))

	for name, env := range map[string]ClientVTEEnv{
		"first": firstEnv, "second": secondEnv,
	} {
		require.Eventuallyf(t, func() bool {
			data, err := env.Config.Clipboard.Paste(clipboard.DefaultRegisterID)
			return err == nil && data.Text == copied
		}, 10*time.Second, 5*time.Millisecond,
			"the %s client's clipboard never received the copied text", name)
	}
}

// TestClientVTEAttachMissingIsTyped pins that a confirmed missing
// emulator survives the wire as workspace.ErrVTENotFound, which is the
// only case where a client may recreate a terminal from a snapshot.
func TestClientVTEAttachMissingIsTyped(t *testing.T) {
	h := newWireHarness(t)
	raw, err := net.Dial("unix", h.addr)
	require.NoError(t, err)

	_, err = NewClientVTE(vtewire.NewConn(raw), &vtewire.Hello{
		VTEID: string(workspace.NewVTEID()), Width: 40, Height: 12,
	}, testClientVTEEnv())
	assert.ErrorIs(t, err, workspace.ErrVTENotFound)
}
