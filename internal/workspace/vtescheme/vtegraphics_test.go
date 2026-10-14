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
	"context"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
)

// scriptedScheme is a workspace whose terminal runs no program: a test
// plays the program, printing the output the canonical emulator reads
// and reading the answers it writes back. Its files are real, so a
// graphics transmission can name one.
type scriptedScheme struct {
	schemeapi.Scheme
	pty *scriptedPty

	mu    sync.Mutex
	sizes []workspaceapi.PtySize
}

// newScriptedServer serves the terminals of a scriptedScheme through
// the same server, factory and listener a workspace server uses. The
// scheme has a single pty, so a test creates a single terminal.
func newScriptedServer(t *testing.T) (*wireHarness, *scriptedScheme) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	uri, err := workspaceapi.CurrentUserHostURI(os.TempDir())
	require.NoError(t, err)
	files, err := workspace.NewFileScheme(ctx, config.NopConfig(), uri)
	require.NoError(t, err)

	r, w := io.Pipe()
	scheme := &scriptedScheme{Scheme: files, pty: &scriptedPty{r: r, w: w}}
	server := NewServer(scheme)
	t.Cleanup(func() { _ = server.Close() })
	return serveWire(t, server), scheme
}

func (s *scriptedScheme) NewPty(context.Context) (workspaceapi.Pty, error) {
	return workspaceapi.Pty{Master: s.pty, Slave: scriptedSlave{s.pty}}, nil
}

func (s *scriptedScheme) SetPtySize(_ workspaceapi.Pty, size workspaceapi.PtySize) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes = append(s.sizes, size)
	return nil
}

func (*scriptedScheme) StartCommand(
	context.Context, workspaceapi.Cmd,
) (workspaceapi.Pid, error) {
	return 0, nil
}

func (*scriptedScheme) Signal(workspaceapi.Pid, syscall.Signal) error { return nil }

// awaitPtySize waits for the pty to be given want.
func (s *scriptedScheme) awaitPtySize(t *testing.T, want workspaceapi.PtySize) {
	t.Helper()
	var last workspaceapi.PtySize
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.sizes) > 0 {
			last = s.sizes[len(s.sizes)-1]
		}
		return last == want
	}, 10*time.Second, 5*time.Millisecond, "pty size is %+v", &last)
}

// scriptedPty is the master of a pty whose slave is the test.
type scriptedPty struct {
	r *io.PipeReader
	w *io.PipeWriter

	closeOnce sync.Once
	mu        sync.Mutex
	input     bytes.Buffer
}

// print writes s as the program's output.
func (p *scriptedPty) print(t *testing.T, s string) {
	t.Helper()
	_, err := p.w.Write([]byte(s))
	require.NoError(t, err)
}

// awaitInput waits for the program's input to contain want and returns
// all of it.
func (p *scriptedPty) awaitInput(t *testing.T, want string) string {
	t.Helper()
	var input string
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		input = p.input.String()
		return strings.Contains(input, want)
	}, 10*time.Second, 5*time.Millisecond, "input is %q", &input)
	return input
}

func (p *scriptedPty) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *scriptedPty) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.Write(b)
}

func (p *scriptedPty) Close() error {
	p.closeOnce.Do(func() { _ = p.r.Close() })
	return nil
}

func (*scriptedPty) Name() string                      { return "/dev/pts/scripted" }
func (*scriptedPty) Stat() (os.FileInfo, error)        { return nil, errNotAFile }
func (*scriptedPty) Sync() error                       { return nil }
func (*scriptedPty) Truncate(int64) error              { return errNotAFile }
func (*scriptedPty) Fd() uintptr                       { return 0 }
func (*scriptedPty) Seek(int64, int) (int64, error)    { return 0, errNotAFile }
func (*scriptedPty) ReadAt([]byte, int64) (int, error) { return 0, errNotAFile }

// scriptedSlave is the program's end of the pty. The emulator closes
// its copy once the program started, which must not end the stream.
type scriptedSlave struct{ *scriptedPty }

func (scriptedSlave) Close() error { return nil }

// canonical is the emulator the server owns for id.
func (h *wireHarness) canonical(t *testing.T, id workspace.VTEID) *canonicalVTE {
	t.Helper()
	c, ok := h.scheme.registry.lookup(h.scheme.root, id)
	require.True(t, ok, "the server has no terminal %s", id)
	return c
}

// displayEnv is the browser of a display whose cells are width by
// height pixels, or of a cells-only display when they are zero.
func displayEnv(width, height int) ClientVTEEnv {
	env := testClientVTEEnv()
	if width > 0 && height > 0 {
		env.Config.CellPixelSize = func() (int, int) { return width, height }
	}
	return env
}

func replicaCursor(v workspace.VTE) term.Coordinates {
	return v.(*clientVTE).Handler.Component().CursorAtScreen()
}

func graphicsAPC(ctl string, payload []byte) string {
	return "\x1b_G" + ctl + ";" + base64.StdEncoding.EncodeToString(payload) + "\x1b\\"
}

// TestServerAnswersPixelQueries reproduces a workspace server leaving
// every pixel query unanswered: its emulator had no cell size, and the
// replicas that do are silent by design, so a program probing for
// graphics support concluded there was none.
func TestServerAnswersPixelQueries(t *testing.T) {
	t.Parallel()

	const graphicsQuery = "\x1b_Gi=31,a=q,s=1,v=1,f=24;AAAA\x1b\\"
	tests := []struct {
		name          string
		width, height int
		query         string
		answer        string
		pty           workspaceapi.PtySize
	}{
		{
			name:  "graphics support",
			width: 10, height: 20,
			query:  graphicsQuery,
			answer: "\x1b_Gi=31;OK\x1b\\",
		},
		{
			name:  "cell size",
			width: 10, height: 20,
			query:  "\x1b[16t",
			answer: "\x1b[6;20;10t",
		},
		{
			name:  "text area size",
			width: 10, height: 20,
			query:  "\x1b[14t",
			answer: "\x1b[4;240;400t",
		},
		{
			name:  "graphics support on a cells-only display",
			query: graphicsQuery,
		},
		{
			name:  "cell size on a cells-only display",
			query: "\x1b[16t",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, scheme := newScriptedServer(t)
			h.attachClientWith(t, &vtewire.Hello{Width: 40, Height: 12},
				displayEnv(tc.width, tc.height))

			// A device status report is always answered, so what came
			// before its answer is everything the query was answered
			// with.
			scheme.pty.print(t, tc.query+"\x1b[5n")
			assert.Equal(t, tc.answer+"\x1b[0n", scheme.pty.awaitInput(t, "\x1b[0n"))
			scheme.awaitPtySize(t, workspaceapi.PtySize{
				Columns: 40, Rows: 12,
				PixelWidth: 40 * tc.width, PixelHeight: 12 * tc.height,
			})
		})
	}
}

// TestReplicasPlaceImagesWhereTheServerDoes reproduces replicas whose
// cursor drifted from the terminal they replicate on every image: each
// placed it with its own display's cell size, the server's emulator
// with none, and placing an image moves the cursor by the cells it
// covers.
func TestReplicasPlaceImagesWhereTheServerDoes(t *testing.T) {
	t.Parallel()

	// 20x40 pixels are 2x2 cells of the owner's display, but 3x3 of the
	// other GUI's.
	image := make([]byte, 20*40*3)
	tests := []struct {
		name string
		// transmit returns the transmission of image, and the file it
		// named, if any.
		transmit func(t *testing.T) (string, string)
		// kept is whether the file survives the transmission.
		kept bool
	}{
		{
			name: "direct",
			transmit: func(*testing.T) (string, string) {
				return graphicsAPC("a=T,f=24,s=20,v=40", image), ""
			},
		},
		{
			name: "file",
			transmit: func(t *testing.T) (string, string) {
				name := writeTemp(t, "img-*.rgb", image)
				return graphicsAPC("a=T,f=24,s=20,v=40,t=f", []byte(name)), name
			},
			kept: true,
		},
		{
			// Only the server may read it: the read deletes it.
			name: "temporary file",
			transmit: func(t *testing.T) (string, string) {
				name := writeTemp(t, "tty-graphics-protocol-*.rgb", image)
				return graphicsAPC("a=T,f=24,s=20,v=40,t=t", []byte(name)), name
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, scheme := newScriptedServer(t)
			owner := h.attachClientWith(t, &vtewire.Hello{Width: 40, Height: 12},
				displayEnv(10, 20))
			id := string(owner.ID())
			replicas := []workspace.VTE{
				owner,
				h.attachClientWith(t, &vtewire.Hello{VTEID: id, Width: 40, Height: 12},
					displayEnv(8, 16)),
				h.attachClientWith(t, &vtewire.Hello{VTEID: id, Width: 40, Height: 12},
					displayEnv(0, 0)),
			}

			transmission, name := tc.transmit(t)
			scheme.pty.print(t, transmission+"x")

			want := term.Coordinates{X: 3, Y: 1}
			comp := h.canonical(t, owner.ID()).h.Component()
			require.Eventually(t, func() bool { return comp.CursorAtScreen() == want },
				10*time.Second, 5*time.Millisecond, "the server's cursor")
			for i, v := range replicas {
				awaitScreen(t, v, "x")
				assert.Equal(t, want, replicaCursor(v), "replica %d", i)
			}
			if name != "" {
				_, err := os.Stat(name)
				assert.Equal(t, tc.kept, err == nil, "stat %s: %v", name, err)
			}
		})
	}
}

// writeTemp writes data to a new file in the temporary directory whose
// name matches pattern, as os.CreateTemp does.
func writeTemp(t *testing.T, pattern string, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp("", pattern)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	_, err = f.Write(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

// TestServerCellSizeFollowsResizeAuthority pins that a cell's size in
// pixels is decided like the grid is: by the client holding resize
// authority, which the first to attach has and focus takes, and which
// passes to the most recently focused client when its holder leaves.
func TestServerCellSizeFollowsResizeAuthority(t *testing.T) {
	t.Parallel()
	h, scheme := newScriptedServer(t)

	first := h.dial(t, &vtewire.Hello{
		Width: 40, Height: 12, CellPixelWidth: 10, CellPixelHeight: 20,
	})
	snap := first.readSnapshot(t)
	assert.Equal(t, 10, snap.Terminal.CellPixelWidth)
	assert.Equal(t, 20, snap.Terminal.CellPixelHeight)
	scheme.awaitPtySize(t, workspaceapi.PtySize{
		Columns: 40, Rows: 12, PixelWidth: 400, PixelHeight: 240,
	})

	second := h.dial(t, &vtewire.Hello{
		VTEID: first.ok.VTEID, Width: 40, Height: 12,
		CellPixelWidth: 8, CellPixelHeight: 16,
	})
	snap = second.readSnapshot(t)
	assert.Equal(t, 10, snap.Terminal.CellPixelWidth,
		"a client without authority is given the cell size in force")

	require.NoError(t, second.conn.WriteFrame(&vtewire.Resize{
		Width: 40, Height: 12, CellPixelWidth: 8, CellPixelHeight: 16,
	}))
	assert.Equal(t, &vtewire.Resize{
		Width: 40, Height: 12, CellPixelWidth: 10, CellPixelHeight: 20,
	}, second.awaitResize(t), "a request that lost is answered with the size in force")

	require.NoError(t, second.conn.WriteFrame(&vtewire.Focus{Focused: true}))
	focused := &vtewire.Resize{
		Width: 40, Height: 12, CellPixelWidth: 8, CellPixelHeight: 16,
	}
	assert.Equal(t, focused, first.awaitResize(t), "focus takes the cell size too")
	assert.Equal(t, focused, second.awaitResize(t))
	scheme.awaitPtySize(t, workspaceapi.PtySize{
		Columns: 40, Rows: 12, PixelWidth: 320, PixelHeight: 192,
	})

	require.NoError(t, second.conn.WriteFrame(&vtewire.Resize{Width: 50, Height: 12}))
	assert.Equal(t, &vtewire.Resize{
		Width: 50, Height: 12, CellPixelWidth: 8, CellPixelHeight: 16,
	}, first.awaitResize(t), "a request without a cell size keeps the one in force")

	require.NoError(t, second.conn.WriteFrame(&vtewire.Detach{}))
	assert.Equal(t, &vtewire.Resize{
		Width: 40, Height: 12, CellPixelWidth: 10, CellPixelHeight: 20,
	}, first.awaitResize(t), "the heir's cell size takes over")
	scheme.awaitPtySize(t, workspaceapi.PtySize{
		Columns: 40, Rows: 12, PixelWidth: 400, PixelHeight: 240,
	})
}

// TestServerReportsTheCellSizeItGranted pins that a client asking for
// a cell size the server bounds is told the one it actually got.
func TestServerReportsTheCellSizeItGranted(t *testing.T) {
	t.Parallel()
	h, _ := newScriptedServer(t)
	client := h.dial(t, &vtewire.Hello{Width: 40, Height: 12})
	client.readSnapshot(t)

	require.NoError(t, client.conn.WriteFrame(&vtewire.Resize{
		Width: 40, Height: 12, CellPixelWidth: 1 << 20, CellPixelHeight: 20,
	}))
	assert.Equal(t, &vtewire.Resize{
		Width: 40, Height: 12, CellPixelWidth: maxCellPixels, CellPixelHeight: 20,
	}, client.awaitResize(t))
}

func TestCellSizeOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		width, height int
		want          cellSize
	}{
		{name: "a cell", width: 10, height: 20, want: cellSize{10, 20}},
		{name: "none", want: cellSize{}},
		{name: "no width", height: 20, want: cellSize{}},
		{name: "no height", width: 10, want: cellSize{}},
		{name: "negative", width: -10, height: -20, want: cellSize{}},
		{
			name:  "implausibly large",
			width: 1 << 40, height: maxCellPixels + 1,
			want: cellSize{maxCellPixels, maxCellPixels},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cellSizeOf(tc.width, tc.height)
			assert.Equal(t, tc.want, got)

			var a atomicCellSize
			a.store(got)
			w, h := a.pixels()
			assert.Equal(t, got, cellSize{w, h})
		})
	}
}

// TestClientVTEAsksForItsDisplayCellSize pins that a client asks for
// its own display's cell size, and asks again when only that changes,
// which is what a new font does.
func TestClientVTEAsksForItsDisplayCellSize(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	cell := [2]int{10, 20}
	env := testClientVTEEnv()
	env.Config.CellPixelSize = func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return cell[0], cell[1]
	}
	v, server, hello := attachToFakeServerWith(t, env, validSnapshot(40, 12))
	assert.Equal(t, 10, hello.CellPixelWidth)
	assert.Equal(t, 20, hello.CellPixelHeight)

	frames := make(chan vtewire.Frame, 8)
	go debug.CapturePanicReport(func() {
		defer close(frames)
		for {
			f, err := server.ReadFrame()
			if err != nil {
				return
			}
			frames <- f
		}
	})

	v.Resize(40, 12)
	mu.Lock()
	cell = [2]int{12, 24}
	mu.Unlock()
	v.Resize(40, 12)

	select {
	case f := <-frames:
		assert.Equal(t, &vtewire.Resize{
			Width: 40, Height: 12, CellPixelWidth: 12, CellPixelHeight: 24,
		}, f, "only the changed cell size is news")
	case <-time.After(5 * time.Second):
		t.Fatal("the changed cell size was never sent")
	}
}

// TestClientVTEPlacesImagesWithTheTerminalsCellSize pins that a
// replica interprets its stream with the cell size of the terminal it
// replicates, whatever its own display's is, and takes each file-backed
// transmission from the server rather than reading it.
func TestClientVTEPlacesImagesWithTheTerminalsCellSize(t *testing.T) {
	t.Parallel()

	// 20x40 pixels are 2x2 cells of 10x20 pixels.
	image := make([]byte, 20*40*3)
	direct := graphicsAPC("a=T,f=24,s=20,v=40", image)
	// The file does not exist: the replica must not look for it.
	file := graphicsAPC("a=T,f=24,s=20,v=40,t=f", []byte("/nonexistent/img.rgb"))
	tests := []struct {
		name     string
		snapshot []byte
		frames   []vtewire.Frame
		want     term.Coordinates
	}{
		{
			name:     "the snapshot's cell size",
			snapshot: snapshotWithCell(40, 12, 10, 20),
			frames:   []vtewire.Frame{&vtewire.Output{Data: []byte(direct + "x")}},
			want:     term.Coordinates{X: 3, Y: 1},
		},
		{
			name:     "a resized cell",
			snapshot: snapshotWithCell(40, 12, 10, 20),
			frames: []vtewire.Frame{
				&vtewire.Resize{Width: 40, Height: 12, CellPixelWidth: 20, CellPixelHeight: 40},
				&vtewire.Output{Data: []byte(direct + "x")},
			},
			want: term.Coordinates{X: 2},
		},
		{
			name:     "a terminal without a cell size",
			snapshot: validSnapshot(40, 12),
			frames:   []vtewire.Frame{&vtewire.Output{Data: []byte(direct + "x")}},
			want:     term.Coordinates{X: 1},
		},
		{
			name:     "a relayed file",
			snapshot: snapshotWithCell(40, 12, 10, 20),
			frames: []vtewire.Frame{
				&vtewire.GraphicsMedium{OK: true, Data: image},
				&vtewire.Output{Data: []byte(file + "x")},
			},
			want: term.Coordinates{X: 3, Y: 1},
		},
		{
			name:     "a file the server could not read",
			snapshot: snapshotWithCell(40, 12, 10, 20),
			frames: []vtewire.Frame{
				&vtewire.GraphicsMedium{},
				&vtewire.Output{Data: []byte(file + "x")},
			},
			want: term.Coordinates{X: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, server, _ := attachToFakeServerWith(t, displayEnv(8, 16), tc.snapshot)
			for _, f := range tc.frames {
				require.NoError(t, server.WriteFrame(f))
			}
			awaitScreen(t, v, "x")
			assert.Equal(t, tc.want, replicaCursor(v))
		})
	}
}
