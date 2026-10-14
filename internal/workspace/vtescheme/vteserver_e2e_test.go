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
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
)

func TestVTEServerAttachStreamsOutput(t *testing.T) {
	h := newWireHarness(t)
	client := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
	require.NotEmpty(t, client.ok.VTEID)
	assert.Equal(t, 40, client.ok.Width)
	assert.Equal(t, 12, client.ok.Height)
	assert.NotEmpty(t, client.ok.URI, "clients identify a terminal by its pty")

	snap := client.readSnapshot(t)
	assert.Equal(t, 40, snap.Terminal.Width)
	assert.Equal(t, 12, snap.Terminal.Height)

	require.NoError(t, client.conn.WriteFrame(
		&vtewire.Input{Data: []byte("echoed by cat\n")}))
	client.awaitOutput(t, "echoed by cat")
}

// TestVTEServerFansOutToEveryClient pins that two attachments see the
// same byte stream, which is what keeps their local emulators in step.
func TestVTEServerFansOutToEveryClient(t *testing.T) {
	h := newWireHarness(t)
	first := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
	first.readSnapshot(t)

	second := h.dial(t, &vtewire.Hello{
		VTEID: first.ok.VTEID, Width: 40, Height: 12,
	})
	assert.Equal(t, first.ok.VTEID, second.ok.VTEID)
	assert.Equal(t, first.ok.URI, second.ok.URI)
	second.readSnapshot(t)

	require.NoError(t, first.conn.WriteFrame(
		&vtewire.Input{Data: []byte("shared line\n")}))
	first.awaitOutput(t, "shared line")
	second.awaitOutput(t, "shared line")
}

// TestVTEServerResizeFollowsFocus pins that the shared pty follows the
// most recently focused client and that every client is told the size
// actually in force.
func TestVTEServerResizeFollowsFocus(t *testing.T) {
	h := newWireHarness(t)
	first := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
	first.readSnapshot(t)
	second := h.dial(t, &vtewire.Hello{
		VTEID: first.ok.VTEID, Width: 40, Height: 12,
	})
	second.readSnapshot(t)

	// The first client still holds authority, so the second's request
	// is answered with the size in force rather than applied.
	require.NoError(t, second.conn.WriteFrame(&vtewire.Resize{Width: 100, Height: 30}))
	assert.Equal(t, &vtewire.Resize{Width: 40, Height: 12},
		second.awaitResize(t))

	// Taking focus takes authority.
	require.NoError(t, second.conn.WriteFrame(&vtewire.Focus{Focused: true}))
	require.NoError(t, second.conn.WriteFrame(&vtewire.Resize{Width: 100, Height: 30}))
	assert.Equal(t, &vtewire.Resize{Width: 100, Height: 30},
		second.awaitResize(t))
	assert.Equal(t, &vtewire.Resize{Width: 100, Height: 30},
		first.awaitResize(t), "every client must learn the new size")
}

// TestVTEServerDetachLeavesEmulatorRunning pins that closing an
// attachment is not closing a terminal.
func TestVTEServerDetachLeavesEmulatorRunning(t *testing.T) {
	h := newWireHarness(t)
	first := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
	first.readSnapshot(t)
	id := first.ok.VTEID

	require.NoError(t, first.conn.WriteFrame(&vtewire.Detach{}))
	require.NoError(t, first.conn.Close())

	again := h.dial(t, &vtewire.Hello{VTEID: id, Width: 40, Height: 12})
	assert.Equal(t, id, again.ok.VTEID)
	again.readSnapshot(t)

	require.NoError(t, again.conn.WriteFrame(
		&vtewire.Input{Data: []byte("still alive\n")}))
	again.awaitOutput(t, "still alive")
}

// TestVTEServerDropsSlowClient pins the backpressure policy: an
// attachment that stops reading is dropped rather than allowed to
// stall the emulator for everyone else, and reattaching by the
// terminal's stable identity resyncs it wholesale.
func TestVTEServerDropsSlowClient(t *testing.T) {
	h := newWireHarness(t)
	slow := h.dial(t, &vtewire.Hello{
		Create: vtewire.CreateOptions{CommandAndArgs: []string{"cat", "/dev/zero"}},
		Width:  40,
		Height: 12,
	})
	slow.readSnapshot(t)
	id := slow.ok.VTEID
	t.Cleanup(func() {
		_ = h.scheme.DeleteVTE(context.Background(), workspace.VTEID(id))
	})

	// A second client that does read measures how much the terminal
	// has produced. Once that is more than one queue's worth, the
	// client that read nothing can only still be attached if the
	// emulator were waiting for it.
	fast := h.dial(t, &vtewire.Hello{VTEID: id, Width: 40, Height: 12})
	fast.readSnapshot(t)
	require.NoError(t, fast.raw.SetReadDeadline(time.Now().Add(60*time.Second)))
	for produced := 0; produced <= 2*vtewire.DefaultQueueBytes; {
		frame, err := fast.conn.ReadFrame()
		require.NoError(t, err)
		if out, ok := frame.(*vtewire.Output); ok {
			produced += len(out.Data)
		}
	}

	require.NoError(t, slow.raw.SetReadDeadline(time.Now().Add(30*time.Second)))
	buf := make([]byte, 64<<10)
	var err error
	for err == nil {
		_, err = slow.raw.Read(buf)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		require.False(t, netErr.Timeout(),
			"a client that stopped reading was never dropped")
	}

	// The terminal itself is untouched: only that one client is gone.
	again := h.dial(t, &vtewire.Hello{VTEID: id, Width: 40, Height: 12})
	assert.Equal(t, id, again.ok.VTEID)
	again.readSnapshot(t)
}

// TestVTEServerSurvivesDegenerateResize pins the attachment socket as a
// trust boundary. Every terminal in a workspace lives in one server, so
// a client asking for a nonsense size must cost that client its own
// size request and nothing else: before the size was bounded, a large
// enough request allocated the grid until the server died, taking every
// other client's terminals with it.
func TestVTEServerSurvivesDegenerateResize(t *testing.T) {
	tests := []struct {
		desc                  string
		width, height         int
		wantWidth, wantHeight int
	}{
		{desc: "zero", width: 0, height: 0, wantWidth: 1, wantHeight: 1},
		{desc: "negative", width: -1, height: -40, wantWidth: 1, wantHeight: 1},
		{
			desc:  "one dimension only",
			width: 0, height: 24,
			wantWidth: 1, wantHeight: 24,
		},
		{
			desc:  "beyond any real display",
			width: 100_000, height: 100_000,
			wantWidth: maxDimension, wantHeight: maxDimension,
		},
		{
			desc:  "int32 extremes",
			width: 1 << 30, height: -(1 << 30),
			wantWidth: maxDimension, wantHeight: 1,
		},
		{
			desc:  "exactly at the bound",
			width: maxDimension, height: maxDimension,
			wantWidth: maxDimension, wantHeight: maxDimension,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			h := newWireHarness(t)
			client := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
			client.readSnapshot(t)

			require.NoError(t, client.conn.WriteFrame(
				&vtewire.Resize{Width: tc.width, Height: tc.height}))

			// The size actually in force comes back in band, the same
			// way it does when another attachment owns the size.
			got := client.awaitResize(t)
			assert.Equal(t, tc.wantWidth, got.Width)
			assert.Equal(t, tc.wantHeight, got.Height)

			// The emulator is still serving this and every other
			// client rather than wedged or dead.
			require.NoError(t, client.conn.WriteFrame(
				&vtewire.Input{Data: []byte("still alive\n")}))
			client.awaitOutput(t, "still alive")
		})
	}
}

// TestVTEServerClampsCreateDimensions pins the same bound on the size a
// HELLO opens with, which is the other way a client names a size.
func TestVTEServerClampsCreateDimensions(t *testing.T) {
	tests := []struct {
		desc                  string
		width, height         int
		wantWidth, wantHeight int
	}{
		{desc: "unset", width: 0, height: 0, wantWidth: 1, wantHeight: 1},
		{desc: "negative", width: -8, height: -8, wantWidth: 1, wantHeight: 1},
		{
			desc:  "implausible",
			width: 1 << 20, height: 1 << 20,
			wantWidth: maxDimension, wantHeight: maxDimension,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			h := newWireHarness(t)
			client := h.dial(t, &vtewire.Hello{
				Width: tc.width, Height: tc.height,
			})
			assert.Equal(t, tc.wantWidth, client.ok.Width)
			assert.Equal(t, tc.wantHeight, client.ok.Height)

			snap := client.readSnapshot(t)
			assert.Equal(t, tc.wantWidth, snap.Terminal.Width)
			assert.Equal(t, tc.wantHeight, snap.Terminal.Height)

			require.NoError(t, client.conn.WriteFrame(
				&vtewire.Input{Data: []byte("created anyway\n")}))
			client.awaitOutput(t, "created anyway")
		})
	}
}

// TestVTEServerRejectsFramesItNeverSends pins the direction rule: the
// server refuses a frame only it is allowed to send rather than acting
// on a client's imitation of one.
func TestVTEServerRejectsWrongDirectionFrames(t *testing.T) {
	tests := []struct {
		desc  string
		frame vtewire.Frame
	}{
		{desc: "hello_ok", frame: &vtewire.HelloOK{VTEID: "x"}},
		{desc: "hello_err", frame: &vtewire.HelloErr{Code: vtewire.CodeNotFound}},
		{desc: "snapshot", frame: &vtewire.Snapshot{Records: []byte{1}}},
		{desc: "output", frame: &vtewire.Output{Data: []byte("fake")}},
		{desc: "history_page", frame: &vtewire.HistoryPage{Page: []byte{1}}},
		{desc: "exit", frame: &vtewire.Exit{Message: "not yours to send"}},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			h := newWireHarness(t)
			client := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
			client.readSnapshot(t)

			require.NoError(t, client.conn.WriteFrame(tc.frame))

			// The server ends the attachment rather than interpreting
			// it; the emulator itself is untouched.
			assertAttachmentClosed(t, client)

			survivor := h.dial(t, &vtewire.Hello{
				VTEID: client.ok.VTEID, Width: 40, Height: 12,
			})
			survivor.readSnapshot(t)
			require.NoError(t, survivor.conn.WriteFrame(
				&vtewire.Input{Data: []byte("outlived it\n")}))
			survivor.awaitOutput(t, "outlived it")
		})
	}
}

// TestVTEServerRejectsSecondHello pins that HELLO opens an attachment
// exactly once: a second one is a client that lost the plot, not a
// re-attach on the same stream.
func TestVTEServerRejectsSecondHello(t *testing.T) {
	h := newWireHarness(t)
	client := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
	client.readSnapshot(t)

	require.NoError(t, client.conn.WriteFrame(&vtewire.Hello{
		Version: vtewire.Version, Width: 10, Height: 10,
	}))
	assertAttachmentClosed(t, client)
}

// TestVTEServerDetachKeepsTheEmulator pins that a client leaving is not
// a decision about the terminal: DETACH ends the attachment only.
func TestVTEServerDetachKeepsTheEmulator(t *testing.T) {
	h := newWireHarness(t)
	client := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
	client.readSnapshot(t)
	require.NoError(t, client.conn.WriteFrame(
		&vtewire.Input{Data: []byte("before detach\n")}))
	client.awaitOutput(t, "before detach")

	require.NoError(t, client.conn.WriteFrame(&vtewire.Detach{}))
	assertAttachmentClosed(t, client)

	again := h.dial(t, &vtewire.Hello{
		VTEID: client.ok.VTEID, Width: 40, Height: 12,
	})
	again.readSnapshot(t)
	require.NoError(t, again.conn.WriteFrame(
		&vtewire.Input{Data: []byte("after detach\n")}))
	again.awaitOutput(t, "after detach")
}

// TestVTEServerHandlesEmptyInput pins that a zero length INPUT is a
// no-op rather than an error or a closed pty.
func TestVTEServerHandlesEmptyInput(t *testing.T) {
	h := newWireHarness(t)
	client := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
	client.readSnapshot(t)

	require.NoError(t, client.conn.WriteFrame(&vtewire.Input{}))
	require.NoError(t, client.conn.WriteFrame(&vtewire.Input{Data: []byte{}}))
	require.NoError(t, client.conn.WriteFrame(
		&vtewire.Input{Data: []byte("still here\n")}))
	client.awaitOutput(t, "still here")
}

// TestAttachmentDelegatesToItsEmulator pins that an attachment a
// server hands out in process reports the canonical emulator itself:
// every client of a terminal sees one interpretation of it.
func TestAttachmentDelegatesToItsEmulator(t *testing.T) {
	v, err := newTestVTERegistry(t).CreateVTE(context.Background(),
		workspace.CreateVTEOptions{Width: 20, Height: 10})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	a := v.(*vteAttachment)
	h := a.c.h

	for _, tc := range []struct {
		name  string
		check func(t *testing.T)
	}{
		{"Draw", func(t *testing.T) { a.Draw(term.NoopWriter{}) }},
		{"Handle", func(t *testing.T) {
			_, handled := a.Handle(term.Event{Type: term.EventKey, Ch: 'x', Raw: []byte("x")})
			assert.True(t, handled)
		}},
		{"Cursor", func(t *testing.T) {
			wantAt, wantStyle, wantOK := h.Cursor()
			at, style, ok := a.Cursor()
			assert.Equal(t, []any{wantAt, wantStyle, wantOK}, []any{at, style, ok})
		}},
		{"Selection", func(t *testing.T) {
			want, wantOK := h.Selection()
			got, ok := a.Selection()
			assert.Equal(t, []any{want, wantOK}, []any{got, ok})
		}},
		{"scrolling", func(t *testing.T) {
			assert.Equal(t, h.SeekUp(), a.SeekUp())
			assert.Equal(t, h.SeekDown(), a.SeekDown())
			assert.Equal(t, h.SeekOffset(), a.SeekOffset())
			assert.Equal(t, h.MaxSeekOffset(), a.MaxSeekOffset())
		}},
		{"SetDefaultAttributes", func(t *testing.T) {
			a.SetDefaultAttributes(term.Attributes{})
		}},
		{"snapshots", func(t *testing.T) {
			snap, err := a.Snapshot()
			require.NoError(t, err)
			assert.NoError(t, a.RestoreFromSnapshot(snap))
		}},
		{"state", func(t *testing.T) {
			assert.Equal(t, h.Component().Title(), a.Title())
			assert.Equal(t, h.Component().URI(), a.URI())
			assert.Equal(t, h.Component().UsedAlternateBuffer(), a.UsedAlternateBuffer())
			assert.Equal(t, h.Component().IsComplete(), a.IsComplete())
		}},
		{"ClearPrimaryBuffer", func(t *testing.T) { a.ClearPrimaryBuffer() }},
	} {
		t.Run(tc.name, tc.check)
	}
}

// TestVTEServerAttachMissingIsTyped pins that a confirmed missing
// emulator is distinguishable from every other failure: it is the only
// case where a client may recreate a terminal from a snapshot.
func TestVTEServerAttachMissingIsTyped(t *testing.T) {
	h := newWireHarness(t)
	raw, err := net.Dial("unix", h.addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	_, err = vtewire.Dial(vtewire.NewConn(raw), &vtewire.Hello{
		VTEID: string(workspace.NewVTEID()), Width: 40, Height: 12,
	})
	var wireErr *vtewire.Error
	require.ErrorAs(t, err, &wireErr)
	assert.True(t, wireErr.NotFound())
}
