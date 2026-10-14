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

package vte

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte/vtesnap"
)

// TestTeeStreamJoinsMidStream is the load-bearing property of the
// attach path: a replica that joins while output is flowing converges
// on the original. Every byte must land in exactly one of the snapshot
// or the tee, so a cut that is off by even one byte diverges here.
func TestTeeStreamJoinsMidStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		desc  string
		chunk func(i int) string
	}{
		{
			desc:  "plain lines",
			chunk: func(i int) string { return fmt.Sprintf("line %d\r\n", i) },
		},
		{
			desc: "styled output",
			chunk: func(i int) string {
				return fmt.Sprintf("\x1b[%dm styled %d \x1b[0m\r\n", 31+i%7, i)
			},
		},
		{
			desc: "cursor addressing",
			chunk: func(i int) string {
				return fmt.Sprintf("\x1b[%d;%dHcell%d", 1+i%10, 1+i%20, i)
			},
		},
		{
			desc: "synchronized updates",
			chunk: func(i int) string {
				return fmt.Sprintf("\x1b[?2026h\x1b[H frame %d \x1b[?2026l", i)
			},
		},
		{
			desc: "wide and combining glyphs",
			chunk: func(i int) string {
				return fmt.Sprintf("한국어 e\u0301 %d\r\n", i)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			origin := newFeedComponent(t, 40, 12)
			replica := newFeedComponent(t, 40, 12)
			pump := newTeePump(t, replica)
			origin.comp.TeeStream(pump.forward)

			// Output before the cut only exists in the snapshot.
			for i := range 20 {
				origin.pty.feed(t, tc.chunk(i))
			}
			origin.settle(t)

			require.NoError(t, origin.comp.SnapshotStream(0,
				func(snapshot []byte, history [][]term.Cell) error {
					decoder, err := vtesnap.NewDecoder(bytes.NewReader(snapshot))
					if err != nil {
						return err
					}
					snap, err := decoder.Ready()
					if err != nil {
						return err
					}
					if err := replica.comp.ApplySnapshot(snap); err != nil {
						return err
					}
					replica.comp.PrependHistory(history)
					if len(snap.Continuation) > 0 {
						pump.forward(snap.Continuation)
					}
					pump.start()
					return nil
				}))

			// Output after the cut only reaches the replica through
			// the tee.
			for i := 20; i < 60; i++ {
				origin.pty.feed(t, tc.chunk(i))
			}
			origin.settle(t)

			pump.drain(t, replica)
			assertSameScreen(t, origin, replica)
		})
	}
}

// TestTeeStreamCutIsAtomic pins that a batch is never both snapshotted
// and teed. The cut runs while a writer floods the emulator, so any
// window between "state captured" and "tee installed" duplicates or
// drops output and shows up as a diverged screen.
func TestTeeStreamCutIsAtomic(t *testing.T) {
	t.Parallel()

	origin := newFeedComponent(t, 40, 12)
	replica := newFeedComponent(t, 40, 12)
	pump := newTeePump(t, replica)
	origin.comp.TeeStream(pump.forward)

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go debug.CapturePanicReport(func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if !assert.NoError(t, origin.pty.put(fmt.Sprintf("busy %d\r\n", i))) {
				return
			}
		}
	})

	// Let the flood get going so the cut has to interrupt it.
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, origin.comp.SnapshotStream(0,
		func(snapshot []byte, history [][]term.Cell) error {
			decoder, err := vtesnap.NewDecoder(bytes.NewReader(snapshot))
			if err != nil {
				return err
			}
			snap, err := decoder.Ready()
			if err != nil {
				return err
			}
			if err := replica.comp.ApplySnapshot(snap); err != nil {
				return err
			}
			replica.comp.PrependHistory(history)
			if len(snap.Continuation) > 0 {
				pump.forward(snap.Continuation)
			}
			pump.start()
			return nil
		}))

	time.Sleep(20 * time.Millisecond)
	close(stop)
	writer.Wait()

	origin.settle(t)
	pump.drain(t, replica)
	assertSameScreen(t, origin, replica)
}

// TestCutStreamWaitsForSynchronizedUpdate pins that a cut never lands
// inside an open synchronized update: those bytes have been received
// but not yet interpreted, so a snapshot taken there would omit output
// the tee has already moved past.
func TestCutStreamWaitsForSynchronizedUpdate(t *testing.T) {
	t.Parallel()

	c := newFeedComponent(t, 40, 12)
	c.comp.TeeStream(func([]byte) {})
	c.feed(t, "\x1b[?2026h"+strings.Repeat("buffered\r\n", 10))

	cut := make(chan error, 1)
	go debug.CapturePanicReport(func() {
		cut <- c.comp.SnapshotStream(5*time.Second,
			func([]byte, [][]term.Cell) error { return nil })
	})

	select {
	case err := <-cut:
		t.Fatalf("cut completed inside a synchronized update: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	c.feed(t, "\x1b[?2026l")
	select {
	case err := <-cut:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("cut never completed after the update closed")
	}
}

// TestCutStreamTimesOut pins that a wedged synchronized update fails
// the attach rather than hanging the caller forever.
func TestCutStreamTimesOut(t *testing.T) {
	t.Parallel()

	c := newFeedComponent(t, 40, 12)
	c.comp.TeeStream(func([]byte) {})
	c.feed(t, "\x1b[?2026hpartial frame")

	err := c.comp.SnapshotStream(20*time.Millisecond,
		func([]byte, [][]term.Cell) error { return nil })
	assert.ErrorIs(t, err, ErrCutTimeout)
}

// TestContinuationRequiresTee pins that continuation tracking is tied
// to teeing: an emulator nobody observes pays nothing for it.
func TestContinuationRequiresTee(t *testing.T) {
	t.Parallel()

	c := newFeedComponent(t, 40, 12)
	c.feed(t, "text\x1b[31m")

	got, err := c.comp.Continuation()
	require.NoError(t, err)
	assert.Nil(t, got, "no tee means no continuation tracking")

	c.comp.TeeStream(func([]byte) {})
	c.feed(t, "more\x1b[3")
	got, err = c.comp.Continuation()
	require.NoError(t, err)
	assert.Equal(t, "\x1b[3", string(got))
}

// TestCutStreamResolvesAnAbandonedSynchronizedUpdate pins that a
// program which opens a synchronized update and then goes quiet — by
// exiting, blocking or crashing — does not make its terminal
// permanently unattachable. Nothing will arrive to close the region,
// so the cut has to resolve it once its timeout has passed.
func TestCutStreamResolvesAnAbandonedSynchronizedUpdate(t *testing.T) {
	t.Parallel()

	c := newFeedComponent(t, 40, 12)
	c.comp.TeeStream(func([]byte) {})
	c.feed(t, "\x1b[?2026habandoned frame")

	// The parser abandons a region 150ms after it opened.
	time.Sleep(200 * time.Millisecond)

	var blob []byte
	require.NoError(t, c.comp.SnapshotStream(time.Second,
		func(snapshot []byte, _ [][]term.Cell) error {
			blob = snapshot
			return nil
		}))

	// The abandoned bytes belong to the snapshot: the tee already
	// delivered them, so a replica must not be told to expect them
	// again.
	replica := newFeedComponent(t, 40, 12)
	decoder, err := vtesnap.NewDecoder(bytes.NewReader(blob))
	require.NoError(t, err)
	snap, err := decoder.Ready()
	require.NoError(t, err)
	require.NoError(t, replica.comp.ApplySnapshot(snap))
	assert.Contains(t, replica.text(), "abandoned frame")
}

// TestContinuationLostWhenTeeJoinsMidSequence pins that a tee
// installed partway through an escape sequence refuses to describe it
// rather than inventing a suffix from bytes whose start it never saw.
func TestContinuationLostWhenTeeJoinsMidSequence(t *testing.T) {
	t.Parallel()

	c := newFeedComponent(t, 40, 12)
	c.feed(t, "text\x1b[3")
	c.comp.TeeStream(func([]byte) {})

	_, err := c.comp.Continuation()
	assert.ErrorIs(t, err, ErrContinuationLost)
	assert.ErrorIs(t,
		c.comp.SnapshotStream(0, func([]byte, [][]term.Cell) error {
			return nil
		}),
		ErrContinuationLost)

	// The parser reaching a fresh start heals tracking.
	c.feed(t, "1m done")
	got, err := c.comp.Continuation()
	require.NoError(t, err)
	assert.Empty(t, got)
}

// teePump stands in for the server's per-attachment send queue: the
// tee runs on the parse goroutine and must never block, so bytes are
// handed to a pump that writes them to the replica.
//
// It also stands in for the server's fan-out: the sink is installed
// before the cut and only starts delivering once the attachment joins,
// because installing a sink from inside the cut would deadlock on the
// feed the cut is holding still.
type teePump struct {
	mu      sync.Mutex
	started bool
	pending [][]byte
	wake    chan struct{}
	done    chan struct{}
	stop    chan struct{}
}

func newTeePump(t *testing.T, replica *feedComponent) *teePump {
	t.Helper()

	p := &teePump{
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
		stop: make(chan struct{}),
	}
	go debug.CapturePanicReport(func() {
		defer close(p.done)
		for {
			select {
			case <-p.wake:
			case <-p.stop:
				p.flush(t, replica)
				return
			}
			p.flush(t, replica)
		}
	})
	t.Cleanup(func() {
		close(p.stop)
		<-p.done
	})
	return p
}

func (p *teePump) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
}

func (p *teePump) forward(batch []byte) {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	p.pending = append(p.pending, bytes.Clone(batch))
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *teePump) flush(t *testing.T, replica *feedComponent) {
	for {
		p.mu.Lock()
		batch := p.pending
		p.pending = nil
		p.mu.Unlock()
		if len(batch) == 0 {
			return
		}
		for _, b := range batch {
			assert.NoError(t, replica.pty.put(string(b)))
		}
	}
}

// drain waits until everything the tee handed over has reached the
// replica's parser.
func (p *teePump) drain(t *testing.T, replica *feedComponent) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		p.mu.Lock()
		empty := len(p.pending) == 0
		p.mu.Unlock()
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tee pump never drained")
		}
		time.Sleep(time.Millisecond)
	}
	replica.settle(t)
}
