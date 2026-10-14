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
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/term/vte/vtesnap"
	"unstable.build/rune/internal/workspace/vtewire"
)

// TestTeeHubSendHistoryPaginates pins how scrollback is cut into
// frames: newest page first, whole rows, and a remaining count that
// reaches zero exactly once.
func TestTeeHubSendHistoryPaginates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rows int
		// wantPages is the row count of each frame in send order.
		wantPages []int
	}{
		{name: "no scrollback sends nothing", rows: 0},
		{name: "one row", rows: 1, wantPages: []int{1}},
		{name: "one row short of a page", rows: 63, wantPages: []int{63}},
		{name: "exactly one page", rows: 64, wantPages: []int{64}},
		{name: "one row past a page", rows: 65, wantPages: []int{64, 1}},
		{name: "two pages exactly", rows: 128, wantPages: []int{64, 64}},
		{name: "several pages", rows: 200, wantPages: []int{64, 64, 64, 8}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			member, frames := newCollectingMember()
			member.sendHistory(numberedRows(tc.rows))

			var gotPages []int
			var gotRows []int
			remaining := -1
			for _, f := range frames() {
				page, ok := f.(*vtewire.HistoryPage)
				require.True(t, ok)
				kind, rows, err := vtesnap.DecodePage(page.Page)
				require.NoError(t, err)
				assert.Equal(t, vtesnap.ScreenPrimary, kind)
				gotPages = append(gotPages, len(rows))
				for _, row := range rows {
					gotRows = append(gotRows, int(row[0].Ch))
				}
				remaining = page.Remaining
			}
			assert.Equal(t, tc.wantPages, gotPages)
			if tc.rows > 0 {
				assert.Zero(t, remaining,
					"the last page must leave nothing outstanding")
			}

			// Newest page first, but rows stay in reading order inside
			// a page: that is what lets a client prepend each page
			// whole.
			var wantRows []int
			for end := tc.rows; end > 0; end -= historyPageRows {
				start := max(end-historyPageRows, 0)
				for i := start; i < end; i++ {
					wantRows = append(wantRows, i)
				}
			}
			assert.Equal(t, wantRows, gotRows)
		})
	}
}

// TestTeeHubBroadcast pins that membership decides delivery and that
// one dead attachment costs the others nothing.
func TestTeeHubBroadcast(t *testing.T) {
	t.Parallel()

	t.Run("every member receives the same frames", func(t *testing.T) {
		t.Parallel()

		h := newTeeHub()
		first, firstFrames := newCollectingMember()
		second, secondFrames := newCollectingMember()
		h.add(first)
		h.add(second)

		h.broadcast(&vtewire.Output{Data: []byte("a")})
		h.broadcast(&vtewire.Resize{Width: 10, Height: 5})
		assert.Len(t, firstFrames(), 2)
		assert.Len(t, secondFrames(), 2)
	})

	t.Run("a removed member stops receiving", func(t *testing.T) {
		t.Parallel()

		h := newTeeHub()
		member, frames := newCollectingMember()
		h.add(member)
		h.broadcast(&vtewire.Output{Data: []byte("a")})
		h.remove(member)
		h.broadcast(&vtewire.Output{Data: []byte("b")})
		assert.Len(t, frames(), 1)
	})

	t.Run("removing twice is not an error", func(t *testing.T) {
		t.Parallel()

		h := newTeeHub()
		member, _ := newCollectingMember()
		h.add(member)
		h.remove(member)
		assert.NotPanics(t, func() { h.remove(member) })
	})

	t.Run("adding twice delivers once", func(t *testing.T) {
		t.Parallel()

		h := newTeeHub()
		member, frames := newCollectingMember()
		h.add(member)
		h.add(member)
		h.broadcast(&vtewire.Output{Data: []byte("a")})
		assert.Len(t, frames(), 1)
	})

	t.Run("an overflowed member is skipped, not retried", func(t *testing.T) {
		t.Parallel()

		h := newTeeHub()
		// A queue this small overflows on the second frame, and the
		// member must then be ignored rather than re-encoded for.
		slow := &teeMember{sender: vtewire.NewSender(
			vtewire.NewConn(nopConn{}), 1)}
		healthy, healthyFrames := newCollectingMember()
		h.add(slow)
		h.add(healthy)

		for range 4 {
			h.broadcast(&vtewire.Output{Data: []byte("payload")})
		}
		assert.True(t, slow.dropped.Load())
		assert.Len(t, healthyFrames(), 4,
			"a dropped attachment must not cost the others their output")
	})
}

// newCollectingMember returns a member whose sender drains into memory,
// together with an accessor for the frames it accepted.
func newCollectingMember() (*teeMember, func() []vtewire.Frame) {
	pipe := newFramePipe()
	member := &teeMember{sender: vtewire.NewSender(vtewire.NewConn(pipe), 0)}
	return member, func() []vtewire.Frame {
		// Send encodes on the caller's goroutine and only queues, so
		// the frames are decoded from the queue rather than waiting on
		// a writer goroutine.
		return pipe.drain(member.sender)
	}
}

// framePipe collects the bytes a sender would write.
type framePipe struct{ buf []byte }

func newFramePipe() *framePipe { return &framePipe{} }

func (p *framePipe) Read([]byte) (int, error) { return 0, io.EOF }

func (p *framePipe) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	return len(b), nil
}

func (p *framePipe) Close() error { return nil }

// drain flushes the sender's queue synchronously and decodes whatever
// reached the connection.
func (p *framePipe) drain(s *vtewire.Sender) []vtewire.Frame {
	s.Close()
	_ = s.Run()

	var out []vtewire.Frame
	r := vtewire.NewConn(&readOnlyConn{buf: p.buf})
	for {
		f, err := r.ReadFrame()
		if err != nil {
			return out
		}
		// A decoded frame aliases the connection's read buffer, which
		// the next read reuses.
		out = append(out, retain(f))
	}
}

func retain(f vtewire.Frame) vtewire.Frame {
	switch v := f.(type) {
	case *vtewire.HistoryPage:
		return &vtewire.HistoryPage{
			Remaining: v.Remaining, Page: bytes.Clone(v.Page)}
	case *vtewire.Output:
		return &vtewire.Output{Data: bytes.Clone(v.Data)}
	default:
		return f
	}
}

type readOnlyConn struct {
	buf []byte
	off int
}

func (c *readOnlyConn) Read(b []byte) (int, error) {
	if c.off >= len(c.buf) {
		return 0, io.EOF
	}
	n := copy(b, c.buf[c.off:])
	c.off += n
	return n, nil
}

func (c *readOnlyConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *readOnlyConn) Close() error                { return nil }

// nopConn discards everything, standing in for a socket whose reader
// has gone away.
type nopConn struct{}

func (nopConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopConn) Write(b []byte) (int, error) { return len(b), nil }
func (nopConn) Close() error                { return nil }

// numberedRows builds scrollback whose first cell holds the row index,
// so a test can assert the order pages were cut in.
func numberedRows(n int) [][]term.Cell {
	if n == 0 {
		return nil
	}
	rows := make([][]term.Cell, n)
	for i := range rows {
		rows[i] = []term.Cell{{Ch: rune(i), Width: 1, Bytes: 1}}
	}
	return rows
}
