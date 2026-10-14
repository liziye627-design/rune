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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/blue/logging"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
)

// cutTimeout bounds how long an attach waits for the emulator to reach
// a resumable point in its byte stream.
const cutTimeout = 2 * time.Second

// maxDimension bounds a pty's columns and rows. The grid is allocated
// eagerly, so an attachment asking for an implausible size would make
// the server that owns every terminal in this workspace allocate until
// it dies. No display comes near this, and a client that asks for more
// is told the size it actually got, exactly as it is when it loses the
// size to another attachment.
const maxDimension = 2048

// clampDimensions keeps a requested size inside what a terminal can
// actually be. Zero and negative sizes reach here from clients that
// have not laid out a window yet.
func clampDimensions(width, height int) (int, int) {
	return min(max(width, 1), maxDimension), min(max(height, 1), maxDimension)
}

// Serve accepts attachment connections until lis is closed, serving
// each under ctx. ctx does not unblock Accept: a caller stops Serve by
// ending ctx and then closing lis, which returns nil. Any other accept
// failure is returned.
//
// Attachments are a raw byte stream between a client's emulator and a
// pty, so they get a listener of their own rather than sharing the
// request/response surface, where terminal lifecycle stays.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go debug.CapturePanicReport(func() {
			if err := s.Attach(ctx, conn); err != nil &&
				!errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.WithField(logging.KeyClass, "vtescheme.Server").
					Warnf("serve vte attachment: %v", err)
			}
		})
	}
}

// Attach serves one attachment over rw, which is how a host that
// already has a byte stream to a client — an ssh channel, a pipe —
// hands it to the terminals this server owns.
func (s *Server) Attach(ctx context.Context, rw net.Conn) error {
	defer func() { _ = rw.Close() }()

	conn := vtewire.NewConn(rw)
	hello, err := vtewire.Accept(conn)
	if err != nil {
		return err
	}

	attachment, err := s.resolve(ctx, hello)
	if err != nil {
		code, message := classifyAttachError(err)
		_ = conn.WriteFrame(&vtewire.HelloErr{Code: code, Message: message})
		return err
	}
	defer func() { _ = attachment.Close() }()

	return newVTESession(conn, attachment).run(hello)
}

// resolve turns a HELLO into an attachment in the workspace it names,
// creating the emulator when the client did not name one.
func (s *Server) resolve(
	ctx context.Context, hello *vtewire.Hello,
) (*vteAttachment, error) {
	root, err := s.workspaceRoot(hello.Root)
	if err != nil {
		return nil, err
	}
	if hello.VTEID != "" {
		vte, err := s.registry.AttachVTE(ctx, root, workspace.VTEID(hello.VTEID))
		if err != nil {
			return nil, err
		}
		return vte.(*vteAttachment), nil
	}

	width, height := clampDimensions(hello.Width, hello.Height)
	return s.registry.create(root, workspace.CreateVTEOptions{
		CommandAndArgs: hello.Create.CommandAndArgs,
		Width:          width,
		Height:         height,
		MaxLines:       hello.Create.MaxLines,
	}, cellSizeOf(hello.CellPixelWidth, hello.CellPixelHeight))
}

func classifyAttachError(err error) (vtewire.ErrCode, string) {
	switch {
	case errors.Is(err, workspace.ErrVTENotFound):
		return vtewire.CodeNotFound, err.Error()
	case errors.Is(err, workspace.ErrVTEUnsupported):
		return vtewire.CodeUnsupported, err.Error()
	default:
		return vtewire.CodeUnknown, err.Error()
	}
}

// vteSession serves one client's attachment: it hands over a snapshot,
// tees live output, and applies the client's input, resizes and focus.
type vteSession struct {
	conn   *vtewire.Conn
	a      *vteAttachment
	member *teeMember
}

func newVTESession(conn *vtewire.Conn, a *vteAttachment) *vteSession {
	return &vteSession{
		conn:   conn,
		a:      a,
		member: &teeMember{sender: vtewire.NewSender(conn, 0)},
	}
}

func (s *vteSession) run(hello *vtewire.Hello) error {
	c := s.a.c
	comp := c.h.Component()

	sendDone := make(chan struct{})
	go debug.CapturePanicReport(func() {
		defer close(sendDone)
		if err := s.member.sender.Run(); err != nil {
			// A dropped or broken attachment must not leave the read
			// loop waiting on a socket nobody is answering. The client
			// reattaches and resyncs from a fresh snapshot.
			s.member.dropped.Store(true)
			_ = s.conn.Close()
		}
	})
	defer func() {
		c.hub.remove(s.member)
		s.member.sender.Close()
		_ = s.conn.Close()
		<-sendDone
	}()

	// The client's size only takes effect if it wins authority, so the
	// reply carries whatever size is actually in force.
	size := c.geometry()
	if hello.Width > 0 && hello.Height > 0 {
		w, h := clampDimensions(hello.Width, hello.Height)
		size = c.resize(s.a, vtewire.Resize{
			Width:           w,
			Height:          h,
			CellPixelWidth:  hello.CellPixelWidth,
			CellPixelHeight: hello.CellPixelHeight,
		})
	}

	var history [][]term.Cell
	err := comp.SnapshotStream(cutTimeout,
		func(snapshot []byte, scrollback [][]term.Cell) error {
			// Queued inside the cut so the first OUTPUT the client
			// receives is the byte immediately after the snapshot.
			s.member.send(&vtewire.HelloOK{
				VTEID:    string(c.id),
				URI:      comp.URI().String(),
				Width:    size.Width,
				Height:   size.Height,
				Complete: comp.IsComplete(),
			})
			s.member.send(&vtewire.Snapshot{Records: snapshot})
			history = scrollback
			c.hub.add(s.member)
			return nil
		})
	if err != nil {
		_ = s.conn.WriteFrame(&vtewire.HelloErr{
			Code:    vtewire.CodeUnknown,
			Message: err.Error(),
		})
		return fmt.Errorf("snapshot emulator: %w", err)
	}

	// Scrollback is queued after the client is already live, so a deep
	// history never delays the screen it belongs to.
	s.member.sendHistory(history)

	exited := make(chan struct{})
	defer close(exited)
	go debug.CapturePanicReport(func() {
		select {
		case <-comp.Exited():
			exit := &vtewire.Exit{}
			if err := comp.ExitErr(); err != nil {
				exit.Err = err.Error()
			}
			s.member.send(exit)
		case <-exited:
		}
	})

	return s.readLoop(comp)
}

func (s *vteSession) readLoop(comp *vte.Component) error {
	for {
		frame, err := s.conn.ReadFrame()
		if err != nil {
			return err
		}
		switch f := frame.(type) {
		case *vtewire.Input:
			// The frame's bytes belong to the read buffer, and the pty
			// write is asynchronous, so this must own its copy.
			if err := comp.WriteToPty(append([]byte(nil), f.Data...)); err != nil {
				return fmt.Errorf("write to pty: %w", err)
			}
		case *vtewire.Resize:
			w, h := clampDimensions(f.Width, f.Height)
			size := s.a.c.resize(s.a, vtewire.Resize{
				Width:           w,
				Height:          h,
				CellPixelWidth:  f.CellPixelWidth,
				CellPixelHeight: f.CellPixelHeight,
			})
			if !grants(size, f) {
				// The request lost to another client's authority; tell
				// this one what the size really is, which is also how
				// a clamped request is reported back.
				s.member.send(&size)
			}
		case *vtewire.Focus:
			s.a.OnFocusChange(f.Focused)
		case *vtewire.Detach:
			return nil
		default:
			return fmt.Errorf("%w: unexpected %s from client",
				vtewire.ErrMalformed, frame.Type())
		}
	}
}

// grants reports whether the size in force is what req asked for.
func grants(size vtewire.Resize, req *vtewire.Resize) bool {
	if size.Width != req.Width || size.Height != req.Height {
		return false
	}
	if cellSizeOf(req.CellPixelWidth, req.CellPixelHeight) == (cellSize{}) {
		return true
	}
	return size.CellPixelWidth == req.CellPixelWidth &&
		size.CellPixelHeight == req.CellPixelHeight
}
