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

package vtescheme

import (
	"bytes"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/programstatus"
	"unstable.build/rune/internal/term/vte/vtesnap"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
)

// attachClient dials the harness and returns a local emulator
// replicating the terminal behind it.
func (h *wireHarness) attachClient(
	t *testing.T, hello *vtewire.Hello,
) workspace.VTE {
	t.Helper()
	return h.attachClientWith(t, hello, testClientVTEEnv())
}

// attachClientWith attaches with caller-supplied collaborators, which
// is how a test observes what a terminal reports to its own UI.
func (h *wireHarness) attachClientWith(
	t *testing.T, hello *vtewire.Hello, env ClientVTEEnv,
) workspace.VTE {
	t.Helper()

	raw, err := net.Dial("unix", h.addr)
	require.NoError(t, err)

	v, err := NewClientVTE(vtewire.NewConn(raw), hello, env)
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	return v
}

// awaitScreen polls the replica's rendered grid until it contains want.
func awaitScreen(t *testing.T, v workspace.VTE, want string) {
	t.Helper()

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		snapshot, err := v.Snapshot()
		require.NoError(c, err)
		assert.Contains(c, screenText(snapshot.ActiveCells()), want)
	}, 10*time.Second, 5*time.Millisecond, "the replica never rendered %q", want)
}

func screenText(rows [][]term.Cell) string {
	var b strings.Builder
	for _, row := range rows {
		for _, c := range row {
			if c.Ch == 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(c.Ch)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// recordingTabs records what a terminal reports to the UI that owns
// it. Every attached client has its own, which is the point: a title
// or an attention mark belongs to the Rune the user is looking at.
type recordingTabs struct {
	mu    sync.Mutex
	names []recordedTabName
}

type recordedTabName struct {
	uri   workspaceapi.URI
	name  string
	attrs term.Attributes
}

func (r *recordingTabs) Tab(
	workspaceapi.URI, rune, string, browserapi.Handler,
) (browserapi.Handler, error) {
	return nil, nil
}

func (r *recordingTabs) SetTabName(
	uri workspaceapi.URI, name string, attrs term.Attributes,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, recordedTabName{uri: uri, name: name, attrs: attrs})
	return nil
}

func (r *recordingTabs) OnTabExit(workspaceapi.URI) bool { return false }

func (r *recordingTabs) SetTabStatus(workspaceapi.URI, programstatus.Report) error {
	return nil
}

func (r *recordingTabs) EndTabStatus(workspaceapi.URI) error { return nil }

func (r *recordingTabs) last() (recordedTabName, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.names) == 0 {
		return recordedTabName{}, false
	}
	return r.names[len(r.names)-1], true
}

// awaitOutputMatch collects OUTPUT bytes until they match re.
func (c *wireClient) awaitOutputMatch(t *testing.T, re *regexp.Regexp) string {
	t.Helper()

	require.NoError(t, c.raw.SetReadDeadline(time.Now().Add(10*time.Second)))
	defer func() { _ = c.raw.SetReadDeadline(time.Time{}) }()

	var seen strings.Builder
	for {
		frame, err := c.conn.ReadFrame()
		require.NoError(t, err, "output so far: %q", seen.String())
		if out, ok := frame.(*vtewire.Output); ok {
			seen.Write(out.Data)
			if re.MatchString(seen.String()) {
				return seen.String()
			}
		}
	}
}

// drainOutput collects whatever arrives within d.
func (c *wireClient) drainOutput(t *testing.T, d time.Duration) string {
	t.Helper()

	require.NoError(t, c.raw.SetReadDeadline(time.Now().Add(d)))
	defer func() { _ = c.raw.SetReadDeadline(time.Time{}) }()

	var seen strings.Builder
	for {
		frame, err := c.conn.ReadFrame()
		if err != nil {
			return seen.String()
		}
		if out, ok := frame.(*vtewire.Output); ok {
			seen.Write(out.Data)
		}
	}
}

// TestClientVTERejectsBadHandshake pins what a client does with a
// server that answers the handshake with something it cannot use. A
// replica has no state of its own to fall back on, so every one of
// these must fail the attach rather than produce a half-built
// emulator.
func TestClientVTERejectsBadHandshake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// reply sends whatever the server answers HELLO with.
		reply  func(*vtewire.Conn)
		wantIs error
	}{
		{
			name: "not found is typed so a caller may recreate",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.HelloErr{
					Code: vtewire.CodeNotFound, Message: "gone"})
			},
			wantIs: workspace.ErrVTENotFound,
		},
		{
			name: "unsupported is typed",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.HelloErr{
					Code: vtewire.CodeUnsupported, Message: "no terminals"})
			},
			wantIs: workspace.ErrVTEUnsupported,
		},
		{
			// A version mismatch says nothing about whether the
			// emulator exists, so it must not read as not-found.
			name: "version mismatch is not mistaken for a missing terminal",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.HelloErr{
					Code: vtewire.CodeVersion, Message: "v2"})
			},
		},
		{
			name: "unknown code stays untyped",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.HelloErr{Code: vtewire.CodeUnknown})
			},
		},
		{
			name: "output where the handshake belongs",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.Output{Data: []byte("hi")})
			},
		},
		{
			name: "hello_ok with an unparseable uri",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.HelloOK{
					VTEID: "v1", URI: "::not a uri::", Width: 40, Height: 12})
				_ = c.WriteFrame(&vtewire.Snapshot{Records: validSnapshot(40, 12)})
			},
		},
		{
			name: "no snapshot after hello_ok",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(helloOK(40, 12))
				_ = c.WriteFrame(&vtewire.Output{Data: []byte("hi")})
			},
		},
		{
			name: "snapshot that is not a snapshot",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(helloOK(40, 12))
				_ = c.WriteFrame(&vtewire.Snapshot{Records: []byte("garbage")})
			},
		},
		{
			name: "snapshot cut before ready",
			reply: func(c *vtewire.Conn) {
				full := validSnapshot(40, 12)
				_ = c.WriteFrame(helloOK(40, 12))
				_ = c.WriteFrame(&vtewire.Snapshot{Records: full[:len(full)/2]})
			},
		},
		{
			name: "connection dropped after hello_ok",
			reply: func(c *vtewire.Conn) {
				_ = c.WriteFrame(helloOK(40, 12))
				_ = c.Close()
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, server := pipeConns(t)
			go debug.CapturePanicReport(func() {
				if _, err := server.ReadFrame(); err != nil {
					return
				}
				tc.reply(server)
			})

			v, err := NewClientVTE(client,
				&vtewire.Hello{Width: 40, Height: 12}, testClientVTEEnv())
			require.Error(t, err)
			assert.Nil(t, v)
			if tc.wantIs != nil {
				assert.ErrorIs(t, err, tc.wantIs)
			} else {
				assert.NotErrorIs(t, err, workspace.ErrVTENotFound,
					"only a confirmed missing emulator may read as not-found")
			}
		})
	}
}

// TestClientVTEEndsTheStreamOnHostileFrames pins that a server frame
// the replica cannot place in the byte stream ends the attachment
// instead of being skipped: silently dropping it would leave this
// replica's screen diverged from every other one.
func TestClientVTEEndsTheStreamOnHostileFrames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		send func(*vtewire.Conn)
	}{
		{
			name: "a client frame arriving from the server",
			send: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.Input{Data: []byte("x")})
			},
		},
		{
			name: "a second handshake",
			send: func(c *vtewire.Conn) { _ = c.WriteFrame(helloOK(40, 12)) },
		},
		{
			name: "a second snapshot",
			send: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.Snapshot{Records: validSnapshot(40, 12)})
			},
		},
		{
			name: "a history page that is not a page record",
			send: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.HistoryPage{Page: []byte("garbage")})
			},
		},
		{
			name: "an exit carrying a failure",
			send: func(c *vtewire.Conn) {
				_ = c.WriteFrame(&vtewire.Exit{Message: "pty vanished"})
			},
		},
		{
			name: "the connection dropped mid-stream",
			send: func(c *vtewire.Conn) { _ = c.Close() },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v, server := attachToFakeServer(t)
			tc.send(server)

			require.Eventually(t, v.IsComplete, 5*time.Second, 5*time.Millisecond,
				"the replica must stop consuming a stream it cannot follow")
		})
	}
}

func TestWirePtyCloseReleasesAReadAwaitingTheSnapshot(t *testing.T) {
	t.Parallel()

	client, _ := pipeConns(t)
	pty := newWirePty(client, "pty")
	read := make(chan error, 1)
	go debug.CapturePanicReport(func() {
		_, err := pty.Read(make([]byte, 8))
		read <- err
	})

	require.NoError(t, pty.Close())
	select {
	case err := <-read:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a closed attachment must not keep its emulator reading")
	}
}

// TestClientVTEAppliesHistoryOldestLast pins the prepend order: pages
// arrive newest first, so each one belongs before the rows already
// held.
func TestClientVTEAppliesHistoryOldestLast(t *testing.T) {
	t.Parallel()

	v, server := attachToFakeServer(t)
	component := v.(*clientVTE).Handler.Component()

	require.NoError(t, server.WriteFrame(&vtewire.HistoryPage{
		Remaining: 2, Page: historyRecord(vtesnap.ScreenPrimary, "n"),
	}))
	require.NoError(t, server.WriteFrame(&vtewire.HistoryPage{
		Remaining: 0, Page: historyRecord(vtesnap.ScreenPrimary, "o"),
	}))
	// Alternate-screen scrollback has nowhere to go on the primary
	// screen and must not be prepended to it.
	require.NoError(t, server.WriteFrame(&vtewire.HistoryPage{
		Remaining: 0, Page: historyRecord(vtesnap.ScreenAlternate, "a"),
	}))

	require.Eventually(t, func() bool {
		return len(component.SnapshotHistory()) == 2
	}, 5*time.Second, 5*time.Millisecond)
	rows := component.SnapshotHistory()
	assert.Equal(t, "o", string(rows[0][0].Ch))
	assert.Equal(t, "n", string(rows[1][0].Ch))
}

// TestClientVTEIgnoresDegenerateServerResize pins that a size the
// server should never send cannot wedge or crash a replica.
func TestClientVTEIgnoresDegenerateServerResize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		width, height int
	}{
		{name: "zero", width: 0, height: 0},
		{name: "zero width", width: 0, height: 12},
		{name: "zero height", width: 40, height: 0},
		{name: "negative", width: -1, height: -1},
		{name: "extreme negative", width: -1 << 31, height: -1 << 31},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v, server := attachToFakeServer(t)
			require.NoError(t, server.WriteFrame(
				&vtewire.Resize{Width: tc.width, Height: tc.height}))
			require.NoError(t, server.WriteFrame(
				&vtewire.Output{Data: []byte("still here")}))

			awaitScreen(t, v, "still here")
		})
	}
}

// TestClientVTEKeepsReflowingAfterExit pins that a process ending is
// news carried by the stream rather than the end of it: the replica
// still renders what the server sends afterwards.
func TestClientVTEKeepsReflowingAfterExit(t *testing.T) {
	t.Parallel()

	v, server := attachToFakeServer(t)
	require.NoError(t, server.WriteFrame(&vtewire.Exit{}))
	require.Eventually(t, v.IsComplete, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, server.WriteFrame(&vtewire.Output{Data: []byte("epilogue")}))
	awaitScreen(t, v, "epilogue")
}

// The replica reports the status the server's process exited with,
// which decides whether its tab is kept to show the failure.
func TestClientVTEReportsTheExitStatus(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		frame   *vtewire.Exit
		wantErr string
	}{
		{frame: &vtewire.Exit{}},
		{frame: &vtewire.Exit{Err: "exit status 3"}, wantErr: "exit status 3"},
	} {
		v, server := attachToFakeServer(t)
		require.NoError(t, server.WriteFrame(tc.frame))
		require.Eventually(t, v.IsComplete, 5*time.Second, 5*time.Millisecond)
		if tc.wantErr == "" {
			assert.NoError(t, v.ExitErr())
		} else {
			assert.EqualError(t, v.ExitErr(), tc.wantErr)
		}
	}
}

// TestClientVTEDoesNotWriteToAClosedAttachment pins that the calls a
// window keeps making after a terminal is closed are inert.
func TestClientVTEDoesNotWriteToAClosedAttachment(t *testing.T) {
	t.Parallel()

	v, _ := attachToFakeServer(t)
	require.NoError(t, v.Close())

	assert.NotPanics(t, func() {
		v.Resize(80, 24)
		v.Resize(0, 0)
		v.OnFocusChange(true)
		v.OnFocusChange(false)
		_ = v.Close()
	})
}

// TestClientVTESendsResizeOnlyOnChange pins that a window redrawing at
// its current size does not put a resize on the shared pty.
func TestClientVTESendsResizeOnlyOnChange(t *testing.T) {
	t.Parallel()

	v, server := attachToFakeServer(t)
	frames := make(chan vtewire.Frame, 8)
	go debug.CapturePanicReport(func() {
		for {
			f, err := server.ReadFrame()
			if err != nil {
				close(frames)
				return
			}
			frames <- f
		}
	})

	// The attach size is already in force, so repeating it is silence.
	v.Resize(40, 12)
	v.Resize(0, 12)
	v.Resize(40, -1)
	v.Resize(80, 24)

	select {
	case f := <-frames:
		resize, ok := f.(*vtewire.Resize)
		require.True(t, ok, "the first frame must be the only real change")
		assert.Equal(t, 80, resize.Width)
		assert.Equal(t, 24, resize.Height)
	case <-time.After(5 * time.Second):
		t.Fatal("the changed size was never sent")
	}
}

// pipeConns returns the two ends of one in-memory attachment.
func pipeConns(t *testing.T) (client, server *vtewire.Conn) {
	t.Helper()

	c, s := net.Pipe()
	client, server = vtewire.NewConn(c), vtewire.NewConn(s)
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// attachToFakeServer completes a handshake by hand and returns the
// replica together with the server end, so a test can send frames no
// real server would.
func attachToFakeServer(t *testing.T) (workspace.VTE, *vtewire.Conn) {
	t.Helper()
	v, server, _ := attachToFakeServerWith(t, testClientVTEEnv(), validSnapshot(40, 12))
	return v, server
}

// attachToFakeServerWith attaches a replica built with env to a fake
// server whose terminal is snapshot, and also returns the HELLO the
// replica sent.
func attachToFakeServerWith(
	t *testing.T, env ClientVTEEnv, snapshot []byte,
) (workspace.VTE, *vtewire.Conn, *vtewire.Hello) {
	t.Helper()

	client, server := pipeConns(t)
	handshake := make(chan error, 1)
	var hello *vtewire.Hello
	go debug.CapturePanicReport(func() {
		frame, err := server.ReadFrame()
		if err != nil {
			handshake <- err
			return
		}
		hello, _ = frame.(*vtewire.Hello)
		if err := server.WriteFrame(helloOK(40, 12)); err != nil {
			handshake <- err
			return
		}
		handshake <- server.WriteFrame(&vtewire.Snapshot{Records: snapshot})
	})

	v, err := NewClientVTE(client, &vtewire.Hello{Width: 40, Height: 12}, env)
	require.NoError(t, err)
	require.NoError(t, <-handshake)
	t.Cleanup(func() { _ = v.Close() })
	require.NotNil(t, hello, "the replica must open with HELLO")
	return v, server, hello
}

func helloOK(width, height int) *vtewire.HelloOK {
	return &vtewire.HelloOK{
		VTEID:  "vte-1",
		URI:    "vte://localhost/dev/pts/1",
		Width:  width,
		Height: height,
	}
}

// validSnapshot is the smallest blob a replica accepts: a terminal, an
// empty primary screen and a cut at a ground state.
func validSnapshot(width, height int) []byte {
	return snapshotWithCell(width, height, 0, 0)
}

// snapshotWithCell is validSnapshot of a terminal whose cells are
// cellWidth by cellHeight pixels.
func snapshotWithCell(width, height, cellWidth, cellHeight int) []byte {
	var buf bytes.Buffer
	e, err := vtesnap.NewEncoder(&buf)
	if err != nil {
		panic(err)
	}
	must(e.Terminal(vtesnap.Terminal{
		Width:           width,
		Height:          height,
		CellPixelWidth:  cellWidth,
		CellPixelHeight: cellHeight,
	}))
	must(e.Screen(vtesnap.Screen{
		Kind: vtesnap.ScreenPrimary, Width: width, Height: height,
		BottomScrollableRegion: height,
	}, nil))
	must(e.Continuation(nil))
	must(e.Ready())
	return buf.Bytes()
}

// historyRecord is one scrollback row whose first cell names it.
func historyRecord(kind vtesnap.ScreenKind, label string) []byte {
	return vtesnap.EncodePage(kind, [][]term.Cell{{
		{Ch: rune(label[0]), Width: 1, Bytes: 1},
	}})
}

func must(err error) {
	if err != nil {
		panic(errors.New("vtescheme test: " + err.Error()))
	}
}
