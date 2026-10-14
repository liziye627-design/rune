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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/term/vte/vteparser"
)

// DefaultCutTimeout bounds how long CutStream waits for the emulator
// to hold no unfinished input. A synchronized update is the only thing
// that can keep it waiting, and the parser abandons one after 150ms.
const DefaultCutTimeout = time.Second

// ErrCutTimeout means the emulator never reached a resumable point.
var ErrCutTimeout = errors.New("vte: timed out waiting for a stream cut point")

// ErrContinuationLost means the emulator's unfinished input outgrew
// the tracker's bound, so its stream position cannot be described.
var ErrContinuationLost = errors.New("vte: continuation tracking was lost")

// streamTee forwards raw pty bytes to an observer and holds the feed
// still while a new observer joins.
//
// The mutex is what makes a cut exact. The parse goroutine holds it
// across "apply this batch, then forward it", and a joining observer
// holds it across "snapshot the emulator, then start receiving". So a
// batch is either fully reflected in the snapshot or fully delivered
// to the tee, never both and never neither.
type streamTee struct {
	mu      sync.Mutex
	sink    func([]byte)
	tracker *vteparser.ContinuationTracker
}

// TeeStream makes fn the sink for this emulator's raw pty bytes.
//
// fn runs on the parse goroutine immediately after the emulator
// consumed a batch, in stream order, and must not block: it is holding
// up the terminal. Installing a tee also starts continuation tracking,
// which CutStream needs to describe the emulator's exact position in
// the stream.
func (t *Component) TeeStream(fn func([]byte)) {
	t.tee.mu.Lock()
	defer t.tee.mu.Unlock()

	t.tee.sink = fn
	if fn == nil || t.tee.tracker != nil {
		return
	}
	tracker := vteparser.NewContinuationTracker(
		vteparser.DefaultContinuationBytes)
	// Tracking starts now, so a sequence already half-parsed cannot be
	// described: its opening bytes were never seen. The tracker heals
	// as soon as the parser reaches a fresh start.
	if !t.parser.Grounded() {
		tracker.MarkBroken()
	}
	t.tee.tracker = tracker
	t.parser.TrackContinuation(tracker)
}

// TeeGraphicsMedia makes fn the sink for what this emulator reads for
// each graphics transmission sent as a file or shared memory object
// (t=f, t=t, t=s), so a replica can load the image without reading it
// again. fn runs on the parse goroutine, before the bytes that carried
// the transmission reach the TeeStream sink, and must not block.
//
// A transmission larger than maxBytes is refused: a replica could not
// be given it, and must fail exactly what this emulator fails.
func (t *Component) TeeGraphicsMedia(maxBytes int, fn func(data []byte, ok bool)) {
	t.relay.tee(maxBytes, fn)
}

// RelayGraphicsMedium gives an observer the outcome the emulator it
// replicates had reading its next file-backed graphics transmission.
// An observer reads no files itself.
func (t *Component) RelayGraphicsMedium(data []byte, ok bool) {
	t.relay.push(data, ok)
}

// WithFeedHeld runs fn between two parser feeds.
//
// Anything fn does to the emulator, and anything it hands to the tee,
// lands at the same point in the byte stream for every observer. That
// is what keeps a resize ordered against the output around it.
//
// fn must not block and must not call TeeStream or CutStream.
func (t *Component) WithFeedHeld(fn func()) {
	t.tee.mu.Lock()
	defer t.tee.mu.Unlock()
	fn()
}

// CutStream runs fn at a point where the emulator has consumed whole
// input and the parse feed is held still, so fn can capture a snapshot
// and start observing the tee at exactly the same byte.
//
// A half-finished escape sequence is fine: the continuation describes
// it. An open synchronized update is not, because its bytes bypass the
// parser entirely, so the cut waits for one to close.
//
// fn must not call TeeStream or WithFeedHeld: the feed is already held
// still. Install one sink up front and let fn register with whatever
// fans it out.
func (t *Component) CutStream(timeout time.Duration, fn func() error) error {
	if timeout <= 0 {
		timeout = DefaultCutTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		t.tee.mu.Lock()
		// A region whose emitter went quiet has no later byte to
		// trigger its replay, so the cut resolves it rather than
		// waiting for output that is not coming.
		t.parser.FlushExpiredSync()
		if !t.parser.SyncPending() {
			defer t.tee.mu.Unlock()
			return fn()
		}
		t.tee.mu.Unlock()

		if time.Now().After(deadline) {
			return ErrCutTimeout
		}
		time.Sleep(time.Millisecond)
	}
}

// Continuation returns the replay-safe bytes describing the emulator's
// position in its byte stream. It is only meaningful inside CutStream,
// which is what holds that position still.
func (t *Component) Continuation() ([]byte, error) {
	if t.tee.tracker == nil {
		return nil, nil
	}
	if t.tee.tracker.Broken() {
		return nil, ErrContinuationLost
	}
	return t.tee.tracker.Bytes(), nil
}

// feed applies one batch of pty output and forwards it to the tee
// under one lock, so observers and the emulator never disagree about
// which bytes have been seen.
func (t *Component) feed(batch []byte) {
	t.tee.mu.Lock()
	t.parser.AdvanceBytes(batch)
	if t.tee.sink != nil {
		t.tee.sink(batch)
	}
	t.tee.mu.Unlock()
	t.version.Add(1)
}

// SnapshotStream is the whole server-side attach step: it cuts the
// byte stream at a resumable point, encodes the emulator through the
// snapshot format's READY marker, copies the scrollback, and runs join
// before releasing the feed.
//
// Whatever join registers begins receiving at the byte the snapshot
// ends on.
func (t *Component) SnapshotStream(
	timeout time.Duration, join func(snapshot []byte, history [][]term.Cell) error,
) error {
	return t.CutStream(timeout, func() error {
		continuation, err := t.Continuation()
		if err != nil {
			return err
		}
		snapshot, err := t.SnapshotThroughReady(continuation)
		if err != nil {
			return fmt.Errorf("encode snapshot: %w", err)
		}
		return join(snapshot, t.SnapshotHistory())
	})
}
