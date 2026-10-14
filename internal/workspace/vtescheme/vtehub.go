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
	"sync"
	"sync/atomic"

	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/term/vte/vtesnap"
	"unstable.build/rune/internal/workspace/vtewire"
)

// historyPageRows is how many scrollback rows one HISTORY_PAGE frame
// carries. Small enough that a client renders history progressively,
// large enough that framing is not the dominant cost.
const historyPageRows = 64

// teeHub fans one emulator's raw pty stream out to every attached
// client.
//
// Membership changes are made while the emulator's feed is held still
// by [vte.Component.CutStream], so a batch is either part of the
// snapshot a joining client received or delivered to it, never both.
// The hub's own mutex only protects the member set; it is never held
// while a client blocks on its socket, because members queue rather
// than write.
type teeHub struct {
	mu      sync.Mutex
	members map[*teeMember]struct{}
}

func newTeeHub() *teeHub {
	return &teeHub{members: make(map[*teeMember]struct{})}
}

// broadcast queues one frame for every member. Callers hold the
// emulator's feed still, so the frame lands at the same stream
// position for every client. It runs on the parse goroutine and must
// not block.
func (h *teeHub) broadcast(f vtewire.Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for m := range h.members {
		m.send(f)
	}
}

// add registers a member. It must be called while the feed is held
// still so the member's first byte is the one after its snapshot.
func (h *teeHub) add(m *teeMember) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.members[m] = struct{}{}
}

func (h *teeHub) remove(m *teeMember) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.members, m)
}

// teeMember is one client's slot in the hub. It owns the send queue so
// a client that stops reading is dropped instead of stalling the
// emulator for everyone else.
type teeMember struct {
	sender *vtewire.Sender
	// dropped is set once the queue overflowed or the socket failed,
	// so the hub stops paying to encode for a dead attachment. It is
	// atomic because the parse goroutine broadcasts while the session
	// goroutine sends frames of its own.
	dropped atomic.Bool
}

func (m *teeMember) send(f vtewire.Frame) {
	if m.dropped.Load() {
		return
	}
	if err := m.sender.Send(f); err != nil {
		m.dropped.Store(true)
	}
}

// sendHistory queues the emulator's scrollback newest page first, so a
// client can prepend each page above what it already renders.
func (m *teeMember) sendHistory(history [][]term.Cell) {
	for end := len(history); end > 0; end -= historyPageRows {
		start := max(end-historyPageRows, 0)
		m.send(&vtewire.HistoryPage{
			Remaining: start,
			Page: vtesnap.EncodePage(
				vtesnap.ScreenPrimary, history[start:end]),
		})
	}
}
