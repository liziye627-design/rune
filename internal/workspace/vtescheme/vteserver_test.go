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
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte/vtesnap"
	"unstable.build/rune/internal/workspace/vtewire"
)

// wireHarness runs the real attachment listener over a real unix
// socket, so tests exercise the same path a client does.
type wireHarness struct {
	scheme *Server
	addr   string
}

func newWireHarness(t *testing.T) *wireHarness {
	t.Helper()
	return serveWire(t, newTestVTERegistry(t))
}

// serveWire serves scheme's attachments the way a workspace server
// does.
func serveWire(t *testing.T, scheme *Server) *wireHarness {
	t.Helper()

	// Unix socket paths are capped near a hundred bytes, well below
	// what t.TempDir names produce.
	dir, err := os.MkdirTemp("", "rv")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr := filepath.Join(dir, "s.sock")
	lis, err := net.Listen("unix", addr)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go debug.CapturePanicReport(func() {
		defer close(served)
		_ = scheme.Serve(ctx, lis)
	})
	t.Cleanup(func() {
		cancel()
		_ = lis.Close()
		<-served
	})
	return &wireHarness{scheme: scheme, addr: addr}
}

// dial opens one attachment and returns it after the handshake.
func (h *wireHarness) dial(t *testing.T, hello *vtewire.Hello) *wireClient {
	t.Helper()

	raw, err := net.Dial("unix", h.addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	conn := vtewire.NewConn(raw)
	ok, err := vtewire.Dial(conn, hello)
	require.NoError(t, err)
	return &wireClient{raw: raw, conn: conn, ok: ok}
}

type wireClient struct {
	raw  net.Conn
	conn *vtewire.Conn
	ok   *vtewire.HelloOK
}

// readSnapshot consumes the SNAPSHOT frame that follows a successful
// handshake.
func (c *wireClient) readSnapshot(t *testing.T) *vtesnap.Snapshot {
	t.Helper()

	frame, err := c.conn.ReadFrame()
	require.NoError(t, err)
	snapshot, ok := frame.(*vtewire.Snapshot)
	require.True(t, ok, "expected SNAPSHOT, got %s", frame.Type())

	decoder, err := vtesnap.NewDecoder(bytes.NewReader(snapshot.Records))
	require.NoError(t, err)
	decoded, err := decoder.Ready()
	require.NoError(t, err)
	return decoded
}

// awaitOutput collects OUTPUT bytes until they contain want.
func (c *wireClient) awaitOutput(t *testing.T, want string) string {
	t.Helper()

	require.NoError(t, c.raw.SetReadDeadline(time.Now().Add(10*time.Second)))
	defer func() { _ = c.raw.SetReadDeadline(time.Time{}) }()

	var seen strings.Builder
	for {
		frame, err := c.conn.ReadFrame()
		require.NoError(t, err, "output so far: %q", seen.String())
		if out, ok := frame.(*vtewire.Output); ok {
			seen.Write(out.Data)
			if strings.Contains(seen.String(), want) {
				return seen.String()
			}
		}
	}
}

// awaitResize returns the next RESIZE frame, ignoring output.
func (c *wireClient) awaitResize(t *testing.T) *vtewire.Resize {
	t.Helper()

	require.NoError(t, c.raw.SetReadDeadline(time.Now().Add(10*time.Second)))
	defer func() { _ = c.raw.SetReadDeadline(time.Time{}) }()

	for {
		frame, err := c.conn.ReadFrame()
		require.NoError(t, err)
		if resize, ok := frame.(*vtewire.Resize); ok {
			return resize
		}
	}
}

// assertAttachmentClosed waits for the server to end the attachment.
func assertAttachmentClosed(t *testing.T, c *wireClient) {
	t.Helper()

	require.NoError(t, c.raw.SetReadDeadline(time.Now().Add(10*time.Second)))
	defer func() { _ = c.raw.SetReadDeadline(time.Time{}) }()

	for {
		frame, err := c.conn.ReadFrame()
		if err != nil {
			var netErr net.Error
			require.False(t, errors.As(err, &netErr) && netErr.Timeout(),
				"the server left the attachment open")
			return
		}
		// Output already in flight when the offending frame arrived is
		// not evidence either way.
		switch frame.(type) {
		case *vtewire.Output, *vtewire.HistoryPage, *vtewire.Resize:
		default:
			t.Fatalf("unexpected %s after an invalid frame", frame.Type())
		}
	}
}
