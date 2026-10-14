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

package vteparser

import (
	"bytes"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
	"unstable.build/rune/internal/term/vte/vtescanner"
)

const (
	// Maximum number of bytes read in one synchronized update (2MiB).
	syncBufferSize = 0x20_0000
	// Maximum time before a synchronized update is aborted.
	syncUpdateTimeout time.Duration = 150 * time.Millisecond

	// Number of bytes in the BSU/ESU CSI sequences.
	syncEscapeLen = 8
)

var (
	// BSU CSI sequence for beginning or extending synchronized updates.
	bsuCSI = []byte("\x1b[?2026h")

	// ESU CSI sequence for terminating synchronized updates.
	esuCSI = []byte("\x1b[?2026l")
)

// SyncState represents the state for synchronized terminal updates.
type syncState struct {
	// Timeout handler for synchronized updates.
	timeout Timeout
	// Bytes read during the synchronized update.
	buffer []byte
}

// parserState represents the internal state for the VTE parser.
type parserState struct {
	// Last processed character for repetition.
	precedingChar rune

	// State for synchronized terminal updates.
	syncState syncState
}

// Parser wraps a Parser to ultimately call methods on a Handler.
type Parser struct {
	state   parserState
	scanner *vtescanner.Scanner
	handler Handler
	// scratch backs the single-byte Advance entry point so it can share
	// the batched synchronized-update path without allocating.
	scratch [1]byte
	// continuation, when set, retains the bytes needed to resume this
	// parser's unfinished state in another emulator.
	continuation *ContinuationTracker
}

// NewParser returns a new Parser instance.
func NewParser(handler Handler, timeout Timeout) *Parser {
	ret := new(Parser)
	ret.Init(handler, timeout)
	return ret
}

// Init initializes this parser with the given Handler
// and Timeout implementations.
func (p *Parser) Init(handler Handler, timeout Timeout) {
	p.handler = handler
	p.state = parserState{
		syncState: syncState{
			timeout: timeout,
		},
	}
	var driver vtescanner.Driver = newDriver(&p.state, handler)
	if log.IsLevelEnabled(log.TraceLevel) {
		driver = vtescanner.WithLoggingDriver(driver)
	}
	p.scanner = vtescanner.NewScanner(driver)
}

// SyncTimeout returns the synchronized update timeout.
func (p *Parser) SyncTimeout() Timeout {
	return p.state.syncState.timeout
}

// TrackContinuation makes the parser retain replay-safe bytes for its
// unfinished state, so a snapshot taken at a feed boundary can be
// resumed byte-exactly elsewhere.
func (p *Parser) TrackContinuation(t *ContinuationTracker) {
	p.continuation = t
}

// Grounded reports whether the parser holds no unfinished state: no
// escape sequence, no partial UTF-8 codepoint and no open synchronized
// update. It is the state in which a continuation is empty.
func (p *Parser) Grounded() bool {
	return p.scanner.State() == vtescanner.Ground && !p.SyncPending()
}

// SyncPending reports whether a synchronized update holds bytes the
// parser has received but not yet interpreted.
//
// Those bytes bypass the scanner, so no continuation describes them: a
// snapshot taken here would omit output that an observer teeing the
// raw stream has already been handed.
func (p *Parser) SyncPending() bool {
	return len(p.state.syncState.buffer) > 0 ||
		p.state.syncState.timeout.PendingTimeout()
}

// Advance processes a new byte from the PTY.
func (p *Parser) Advance(ch byte) {
	if p.state.syncState.timeout.PendingTimeout() {
		p.scratch[0] = ch
		p.advanceSync(p.scratch[:])
		return
	}
	p.replayTimedOutSync()
	p.scanner.Advance(ch)
	if p.continuation != nil {
		p.scratch[0] = ch
		p.continuation.track(p.scanner.State(), p.scratch[:])
	}
}

// AdvanceBytes processes a batch of bytes from the PTY. Runs of
// printable text encountered while the scanner is in ground state are
// delivered to the handler in one InputRun call instead of per-byte
// dispatch, which dominates bulk output streams.
func (p *Parser) AdvanceBytes(buf []byte) {
	for len(buf) > 0 {
		if p.state.syncState.timeout.PendingTimeout() {
			buf = buf[p.advanceSync(buf):]
			continue
		}
		p.replayTimedOutSync()
		buf = buf[p.advanceRuns(buf, true):]
	}
}

// replayTimedOutSync interprets the bytes of a synchronized update
// whose timeout passed before its end marker arrived. Called before
// any later output, which must not overtake them.
func (p *Parser) replayTimedOutSync() {
	if len(p.state.syncState.buffer) > 0 {
		p.StopSync()
	}
}

// advanceRuns feeds buf through the scanner, handing each printable run
// to the handler in a single InputRun call, and reports how many bytes
// it consumed. With stopOnSync it returns as soon as a synchronized
// update opens so the caller can buffer the remainder instead; StopSync
// clears it because re-entering the buffer it is draining would corrupt
// that buffer.
func (p *Parser) advanceRuns(buf []byte, stopOnSync bool) int {
	consumed := 0
	for consumed < len(buf) {
		rest := buf[consumed:]
		if n := p.scanner.GroundRun(rest); n > 0 {
			p.handler.InputRun(rest[:n])
			p.state.precedingChar, _ = utf8.DecodeLastRune(rest[:n])
			consumed += n
			continue
		}
		p.scanner.Advance(rest[0])
		consumed++
		if stopOnSync && p.state.syncState.timeout.PendingTimeout() {
			break
		}
	}
	if p.continuation != nil {
		p.continuation.track(p.scanner.State(), buf[:consumed])
	}
	return consumed
}

// StopSync ends a synchronized update.
func (p *Parser) StopSync() {
	p.stopSync(true)
}

// FlushExpiredSync replays a synchronized update whose timeout has
// passed and reports whether it had anything to replay.
//
// An emitter that opens a region and then stops producing output — by
// exiting, blocking or crashing — leaves those bytes buffered with no
// later byte to trigger the replay, which would otherwise keep the
// parser permanently short of a resumable stream position.
func (p *Parser) FlushExpiredSync() bool {
	if p.state.syncState.timeout.PendingTimeout() ||
		len(p.state.syncState.buffer) == 0 {
		return false
	}
	p.StopSync()
	return true
}

func (p *Parser) stopSync(reportEnd bool) {
	// Buffered bytes are ordinary terminal output: replay them through
	// the same batched path AdvanceBytes uses so a synchronized frame is
	// not charged per-byte dispatch on top of the buffering pass.
	p.advanceRuns(p.state.syncState.buffer, false)

	if reportEnd {
		// Timeout and overflow have no ESU for the driver to dispatch.
		p.handler.UnsetPrivateMode(PrivateModeSyncUpdate)
	}
	// Resetting state after processing makes sure we don't interpret buffered sync escapes.
	p.state.syncState.buffer = p.state.syncState.buffer[:0]
	p.state.syncState.timeout.ClearTimeout()
}

// SyncBytesCount returns the number of bytes in the synchronization buffer.
func (p *Parser) SyncBytesCount() int {
	return len(p.state.syncState.buffer)
}

// advanceSync buffers the leading bytes of buf that belong to the open
// synchronized update and reports how many it consumed. The BSU/ESU
// terminators are located with one scan over the appended bytes rather
// than a suffix comparison after every byte.
//
// NOTE: It is technically legal to specify multiple private modes in the
// same escape, but we only recognize EXACTLY `\e[?2026h`/`\e[?2026l` to
// keep the parser reasonable.
func (p *Parser) advanceSync(buf []byte) int {
	sync := &p.state.syncState
	if sync.buffer == nil {
		sync.buffer = make([]byte, 0, syncBufferSize)
	}
	prevLen := len(sync.buffer)
	// The region is flushed as soon as it reaches syncBufferSize-1, so
	// the buffer never outgrows its initial capacity.
	n := min(len(buf), syncBufferSize-1-prevLen)
	sync.buffer = append(sync.buffer, buf[:n]...)

	// Only the last syncEscapeLen-1 already-buffered bytes can begin a
	// marker that completes within the bytes just appended, so anything
	// before that was already classified by an earlier call.
	at := max(prevLen-(syncEscapeLen-1), 0)
	for {
		i := bytes.IndexByte(sync.buffer[at:], c0ESC)
		if i < 0 {
			break
		}
		at += i
		if at+syncEscapeLen > len(sync.buffer) {
			// A marker split across batches: re-examined from here once
			// the rest of it arrives.
			break
		}
		switch seq := sync.buffer[at : at+syncEscapeLen]; {
		case bytes.Equal(seq, bsuCSI):
			sync.timeout.SetTimeout(syncUpdateTimeout)
			at += syncEscapeLen
		case bytes.Equal(seq, esuCSI):
			end := at + syncEscapeLen
			sync.buffer = sync.buffer[:end]
			// Replaying ESU resets any incomplete scanner state in the body
			// and dispatches the normal mode transition exactly once.
			p.stopSync(false)
			return end - prevLen
		default:
			at++
		}
	}

	if len(sync.buffer) >= syncBufferSize-1 {
		p.StopSync()
	}
	return n
}
