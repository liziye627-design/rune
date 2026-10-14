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
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/browser"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/term/vte/vtesnap"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
)

// ClientVTEEnv are the collaborators a replicated terminal needs from
// the client hosting it.
//
// They are client-local by nature: a bell, a notification and a tab
// title belong to the Rune the user is looking at, not to the server
// that owns the pty. Supplying them here is what lets every attached
// client react to the same byte stream in its own UI.
type ClientVTEEnv struct {
	Publisher     browser.EventPublisher
	Notifications browser.Notifications
	Tabs          browser.TabManager
	Config        vte.Config
}

// clientVTEEnvKey is the context key the collaborators travel under.
type clientVTEEnvKey struct{}

// WithClientVTEEnv returns a context carrying the collaborators a
// terminal opened through it should use.
//
// They travel with the call rather than with the scheme because they
// belong to the window the terminal is opened in: one scheme serves
// every window of a workspace, and each has its own tabs,
// notifications and event loop.
func WithClientVTEEnv(ctx context.Context, env ClientVTEEnv) context.Context {
	return context.WithValue(ctx, clientVTEEnvKey{}, env)
}

// ClientVTEEnvFrom reads the collaborators [WithClientVTEEnv] put on
// ctx.
func ClientVTEEnvFrom(ctx context.Context) (ClientVTEEnv, error) {
	env, ok := ctx.Value(clientVTEEnvKey{}).(ClientVTEEnv)
	if !ok {
		return ClientVTEEnv{}, fmt.Errorf(
			"%w: no terminal host on this call", workspace.ErrVTEUnsupported)
	}
	if env.Publisher == nil || env.Notifications == nil || env.Tabs == nil {
		return ClientVTEEnv{}, fmt.Errorf(
			"%w: terminal host is incomplete", workspace.ErrVTEUnsupported)
	}
	return env, nil
}

// NewClientVTE attaches to a terminal a workspace server owns and
// returns a local emulator replaying its byte stream.
//
// conn must be freshly connected and unused: the handshake, the
// snapshot and the live stream are all read from it, in that order.
// The returned value owns conn and closes it on Close.
func NewClientVTE(
	conn *vtewire.Conn, hello *vtewire.Hello, env ClientVTEEnv,
) (workspace.VTE, error) {
	display := displayCellSize(env.Config)
	if cellSizeOf(hello.CellPixelWidth, hello.CellPixelHeight) == (cellSize{}) {
		cell := display()
		hello.CellPixelWidth, hello.CellPixelHeight = cell.width, cell.height
	}
	ok, err := vtewire.Dial(conn, hello)
	if err != nil {
		_ = conn.Close()
		return nil, attachError(err)
	}
	snap, err := readSnapshot(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	uri, err := workspaceapi.ParseURI(ok.URI)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("parse terminal uri %q: %w", ok.URI, err)
	}

	v := &clientVTE{
		id:      workspace.VTEID(ok.VTEID),
		uri:     uri,
		conn:    conn,
		display: display,
		width:   ok.Width,
		height:  ok.Height,
		asked:   cellSizeOf(hello.CellPixelWidth, hello.CellPixelHeight),
	}
	v.cell.store(cellSizeOf(
		snap.Terminal.CellPixelWidth, snap.Terminal.CellPixelHeight))
	v.pty = newWirePty(conn, uri.Path())

	cfg := env.Config
	cfg.Observer = true
	cfg.URI = uri
	cfg.WidthHint = ok.Width
	cfg.HeightHint = ok.Height
	// The replica interprets the stream with the cell size the terminal
	// it replicates has, not this display's: placing an image moves the
	// cursor by the cells it covers. Images are sized in cells, so they
	// still draw at this display's size.
	cfg.CellPixelSize = v.cell.pixels
	if snap.Terminal.MaxScrollback > 0 {
		cfg.MaxLines = snap.Terminal.MaxScrollback
	}
	// A replica interprets a stream someone else produced, so nothing
	// here may resolve or start a program.
	cfg.CommandAndArgs = nil
	cfg.CommandExpander = nil
	cfg.Watcher = nil

	h, err := vte.NewHandler(env.Publisher, env.Notifications,
		wireTerminal{pty: v.pty}, nil, env.Tabs, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("attach terminal %s: %w", ok.VTEID, err)
	}
	v.Handler = h
	v.schedule = cfg.ScheduleNextTick

	if err := h.ApplySnapshot(snap); err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("restore terminal %s: %w", ok.VTEID, err)
	}

	// The emulator only starts consuming the stream once it holds the
	// state that stream continues from.
	v.pty.v = v
	close(v.pty.ready)
	return v, nil
}

// attachError maps a rejected handshake onto the scheme-level errors
// callers already branch on, so a confirmed missing emulator stays
// distinguishable from a transport failure.
func attachError(err error) error {
	var wireErr *vtewire.Error
	if !errors.As(err, &wireErr) {
		return err
	}
	switch wireErr.Code {
	case vtewire.CodeNotFound:
		return fmt.Errorf("%w: %s", workspace.ErrVTENotFound, wireErr.Message)
	case vtewire.CodeUnsupported:
		return fmt.Errorf("%w: %s", workspace.ErrVTEUnsupported, wireErr.Message)
	default:
		return err
	}
}

func readSnapshot(conn *vtewire.Conn) (*vtesnap.Snapshot, error) {
	frame, err := conn.ReadFrame()
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	snapshot, ok := frame.(*vtewire.Snapshot)
	if !ok {
		return nil, fmt.Errorf("%w: expected SNAPSHOT, got %s",
			vtewire.ErrMalformed, frame.Type())
	}
	decoder, err := vtesnap.NewDecoder(bytes.NewReader(snapshot.Records))
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	snap, err := decoder.Ready()
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return snap, nil
}

// displayCellSize reads the size of a cell on the display cfg belongs
// to, which is none when that display cannot draw images.
func displayCellSize(cfg vte.Config) func() cellSize {
	if cfg.CellPixelSize == nil {
		return func() cellSize { return cellSize{} }
	}
	return func() cellSize { return cellSizeOf(cfg.CellPixelSize()) }
}

// clientVTE is one client's attachment to a terminal owned by a
// workspace server.
//
// The emulator is real and local: it parses the canonical byte stream
// itself, so bells, notifications, titles and selections happen here
// rather than being rendered copies of decisions made elsewhere. Only
// authority over the shared pty — its size and its input — belongs to
// the server.
type clientVTE struct {
	*vte.Handler
	id       workspace.VTEID
	uri      workspaceapi.URI
	conn     *vtewire.Conn
	pty      *wirePty
	schedule func(func()) bool
	// display is the size of a cell on this client's display, which it
	// asks the terminal to take.
	display func() cellSize
	// cell is the size of a cell in force for the terminal.
	cell atomicCellSize

	mu            sync.Mutex
	width, height int
	// asked is the display's cell size last sent to the server.
	asked  cellSize
	closed bool
}

var _ workspace.VTE = (*clientVTE)(nil)

// ID satisfies workspace.VTE.
func (v *clientVTE) ID() workspace.VTEID { return v.id }

// URI satisfies workspace.VTE.
func (v *clientVTE) URI() workspaceapi.URI { return v.uri }

// Title satisfies workspace.VTE.
func (v *clientVTE) Title() string { return v.Handler.Component().Title() }

// IsComplete satisfies workspace.VTE.
func (v *clientVTE) IsComplete() bool { return v.Handler.Component().IsComplete() }

// UsedAlternateBuffer satisfies workspace.VTE.
func (v *clientVTE) UsedAlternateBuffer() bool {
	return v.Handler.Component().UsedAlternateBuffer()
}

// Resize satisfies tui.Component. It is a request: the shared pty
// follows one client at a time, so the local emulator only resizes
// when the server says the size changed.
//
// A new font resizes a window without changing its grid, so the size
// of this display's cells is asked for too.
func (v *clientVTE) Resize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	cell := v.display()
	v.mu.Lock()
	same := v.width == width && v.height == height && v.asked == cell
	v.width, v.height = width, height
	v.asked = cell
	closed := v.closed
	v.mu.Unlock()
	if same || closed {
		return
	}
	_ = v.conn.WriteFrame(&vtewire.Resize{
		Width:           width,
		Height:          height,
		CellPixelWidth:  cell.width,
		CellPixelHeight: cell.height,
	})
}

// OnFocusChange satisfies workspace.VTE. Focus decides whose size the
// shared pty follows, so the server is told as well as the emulator.
func (v *clientVTE) OnFocusChange(inFocus bool) {
	v.Handler.OnFocusChange(inFocus)
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if !closed {
		_ = v.conn.WriteFrame(&vtewire.Focus{Focused: inFocus})
	}
}

// Close detaches this client. The terminal, its pty and its process
// keep running in the workspace.
func (v *clientVTE) Close() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	v.mu.Unlock()

	// DETACH only lets the server drop the attachment before the
	// socket does. Closing runs on the host loop, so a server that
	// has stopped reading must not be able to hold a window open:
	// closing the handler closes the connection under this write.
	go debug.CapturePanicReport(func() {
		_ = v.conn.WriteFrame(&vtewire.Detach{})
	})
	return v.Handler.Close()
}

// applyResize reflows the local emulator at the point in the byte
// stream the server reflowed at, then lets the host loop catch up with
// the handler-level state that belongs to it.
func (v *clientVTE) applyResize(f *vtewire.Resize) {
	width, height := f.Width, f.Height
	if width <= 0 || height <= 0 {
		return
	}
	v.mu.Lock()
	v.width, v.height = width, height
	v.mu.Unlock()

	v.cell.store(cellSizeOf(f.CellPixelWidth, f.CellPixelHeight))
	if err := v.Handler.Component().Resize(width, height); err != nil {
		return
	}
	v.schedule(func() { v.Handler.Resize(width, height) })
}

func (v *clientVTE) applyHistory(page []byte) error {
	kind, rows, err := vtesnap.DecodePage(page)
	if err != nil {
		return err
	}
	if kind != vtesnap.ScreenPrimary {
		return nil
	}
	v.Handler.Component().PrependHistory(rows)
	return nil
}

// wireTerminal presents the attachment as a terminal provider so the
// emulator can be built the same way a local one is.
//
// The pty's size is the server's business: it is shared by every
// client, so a replica must never set it directly.
type wireTerminal struct{ pty *wirePty }

// NewPty satisfies schemeapi.Terminal.
func (t wireTerminal) NewPty(context.Context) (workspaceapi.Pty, error) {
	return workspaceapi.Pty{Master: t.pty, Slave: t.pty}, nil
}

// SetPtySize satisfies schemeapi.Terminal.
func (wireTerminal) SetPtySize(workspaceapi.Pty, workspaceapi.PtySize) error { return nil }

// wirePty is the attachment seen as a pty: reads are the canonical
// output stream, writes are this client's input.
//
// Reads dispatch every other frame in stream order, so a resize or a
// scrollback page lands at the same point in the byte stream for this
// replica as it did for the terminal it replicates.
type wirePty struct {
	conn *vtewire.Conn
	name string
	// ready is closed once the emulator holds the state the stream
	// continues from; closed is closed with the attachment, which
	// releases a read that will never get there.
	ready  chan struct{}
	closed chan struct{}
	v      *clientVTE

	buf       []byte
	pending   []byte
	closeOnce sync.Once
	closeErr  error
}

var _ workspaceapi.File = (*wirePty)(nil)

func newWirePty(conn *vtewire.Conn, name string) *wirePty {
	return &wirePty{
		conn:   conn,
		name:   name,
		ready:  make(chan struct{}),
		closed: make(chan struct{}),
	}
}

// Read satisfies io.Reader.
func (p *wirePty) Read(b []byte) (int, error) {
	select {
	case <-p.ready:
	case <-p.closed:
		return 0, net.ErrClosed
	}
	for {
		if len(p.pending) > 0 {
			n := copy(b, p.pending)
			p.pending = p.pending[n:]
			return n, nil
		}
		frame, err := p.conn.ReadFrame()
		if err != nil {
			return 0, err
		}
		switch f := frame.(type) {
		case *vtewire.Output:
			p.buf = append(p.buf[:0], f.Data...)
			p.pending = p.buf
		case *vtewire.Resize:
			p.v.applyResize(f)
		case *vtewire.GraphicsMedium:
			// The frame's bytes belong to the read buffer, and the
			// emulator keeps them until it parses the transmission.
			p.v.Handler.Component().RelayGraphicsMedium(bytes.Clone(f.Data), f.OK)
		case *vtewire.HistoryPage:
			if err := p.v.applyHistory(f.Page); err != nil {
				return 0, err
			}
		case *vtewire.Exit:
			// The process ending is news carried by the stream, not
			// the end of it: the terminal still reflows.
			var exitErr error
			if f.Err != "" {
				exitErr = errors.New(f.Err)
			}
			p.v.Handler.MarkExited(exitErr)
			if f.Message != "" {
				return 0, errors.New(f.Message)
			}
		default:
			return 0, fmt.Errorf("%w: unexpected %s from server",
				vtewire.ErrMalformed, frame.Type())
		}
	}
}

// Write satisfies io.Writer.
func (p *wirePty) Write(b []byte) (int, error) {
	if err := p.conn.WriteFrame(&vtewire.Input{Data: b}); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close satisfies io.Closer. The emulator closes master and slave
// separately, and both are this one connection.
func (p *wirePty) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.closeErr = p.conn.Close()
	})
	return p.closeErr
}

// Name satisfies workspaceapi.File.
func (p *wirePty) Name() string { return p.name }

// Stat satisfies workspaceapi.File.
func (p *wirePty) Stat() (os.FileInfo, error) { return nil, errNotAFile }

// Sync satisfies workspaceapi.File.
func (p *wirePty) Sync() error { return nil }

// Truncate satisfies workspaceapi.File.
func (p *wirePty) Truncate(int64) error { return errNotAFile }

// Fd satisfies workspaceapi.File.
func (p *wirePty) Fd() uintptr { return 0 }

// Seek satisfies io.Seeker.
func (p *wirePty) Seek(int64, int) (int64, error) { return 0, errNotAFile }

// ReadAt satisfies io.ReaderAt.
func (p *wirePty) ReadAt([]byte, int64) (int, error) { return 0, errNotAFile }

// errNotAFile rejects the file operations a byte stream cannot answer.
var errNotAFile = errors.New("vtescheme: terminal attachment is not a file")
