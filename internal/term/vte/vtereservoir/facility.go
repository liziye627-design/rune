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

package vtereservoir

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ernestrc/go-multierror"
	"github.com/ernestrc/logd-go/logging"
	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
)

// Terminals is the subset of schemeapi.Scheme the reservoir needs: it
// warms terminals the workspace owns rather than building emulators of
// its own, so a pooled terminal is already persistent by the time a
// caller takes it.
type Terminals interface {
	CreateVTE(context.Context, workspace.CreateVTEOptions) (workspace.VTE, error)
	DeleteVTE(context.Context, workspace.VTEID) error
}

// Facility is a pool of vte instances.It only caches vte instances
// that start with no initial commands,
type Facility struct {
	mu        sync.Mutex
	cond      *sync.Cond
	pool      []workspace.VTE
	closed    bool
	new       func() (workspace.VTE, error)
	terminals Terminals
	// pendingInit counts how many VTEs are still being created by
	// initCap. Get() blocks on cond while the pool is empty and
	// pendingInit > 0 so callers do not race the warm-up goroutines.
	pendingInit     int
	initialCapacity int

	// ctx and cancelCtx bound the lifetime of the remote-scheme
	// invalidation watcher started by New; cancelCtx is invoked
	// from Close so the watcher goroutine exits.
	ctx       context.Context
	cancelCtx context.CancelFunc
}

// defaultSpawnTimeout bounds the CreateVTE request that builds a
// pooled VTE. Remote spawns run off the host event
// loop (behind the asyncVTE placeholder), so the bound only keeps
// background attempts finite: a spawn against a wedged transport
// fails after the timeout so the placeholder shows an error instead
// of spinning forever.
const defaultSpawnTimeout = 30 * time.Second

// disposeTimeout bounds the DeleteVTE issued for a warm terminal
// nobody claimed. It is deliberately independent of the facility
// context, which is already cancelled by the time Close disposes.
const disposeTimeout = 5 * time.Second

// New allocates storage for a new Facility and initializes it.
func New(
	terminals Terminals, transport schemeapi.Terminal,
	config vte.Config, initialCapacity int,
) *Facility {
	if config.WidthHint == 0 {
		config.WidthHint = 100
	}
	if config.HeightHint == 0 {
		config.HeightHint = config.WidthHint / 2
	}
	if config.SpawnTimeout == 0 {
		config.SpawnTimeout = defaultSpawnTimeout
	}
	rs, _ := transport.(workspace.RemoteScheme)
	newVTE := func(ret *Facility) (workspace.VTE, error) {
		if rs != nil {
			// SpawnTimeout must measure the spawn RPCs, not
			// transport establishment: first-connect provisioning
			// is unbounded (it may install packages on the remote
			// host), and remote scheme calls block until the first
			// connection attempt settles. Racing the budget against
			// that wait would spuriously fail the warm-up right as
			// the workspace becomes usable.
			if err := rs.WaitConnected(ret.ctx); err != nil {
				return nil, fmt.Errorf("wait for remote transport: %w", err)
			}
		}
		ret.log(log.TraceLevel, "called pool.New, width hint: %d, height hint: %d",
			config.WidthHint, config.HeightHint)
		ctx, cancel := context.WithTimeout(ret.ctx, config.SpawnTimeout)
		defer cancel()
		return terminals.CreateVTE(ctx, workspace.CreateVTEOptions{
			CommandAndArgs: config.CommandAndArgs,
			Width:          config.WidthHint,
			Height:         config.HeightHint,
			MaxLines:       config.MaxLines,
		})
	}
	return newWithFactory(terminals, newVTE, transport, initialCapacity)
}

// newWithFactory builds a Facility around a caller-supplied VTE
// factory. Tests use it to inject in-memory VTEs without spinning
// up a real schemeapi.Terminal; production callers go through
// [New], whose factory creates each terminal with
// [Terminals.CreateVTE].
//
// Keeping the factory injection on a single internal entrypoint
// means the lifecycle wiring (cond, ctx/cancelCtx, pendingInit,
// remote-scheme watcher) lives in exactly one place — tests
// exercise the same path real callers do, so a regression in
// New's wiring shows up in unit tests rather than only in
// integration.
//
// initCap runs asynchronously (matching production semantics).
// Callers that need to observe a fully warmed pool synchronously
// — primarily tests asserting allocation counts — must wait for
// it explicitly; see [Facility.WaitForInitialFill].
func newWithFactory(
	terminals Terminals,
	newVTE func(*Facility) (workspace.VTE, error),
	transport schemeapi.Terminal,
	initialCapacity int,
) *Facility {
	ret := new(Facility)
	ret.cond = sync.NewCond(&ret.mu)
	ret.terminals = terminals
	ret.initialCapacity = initialCapacity
	ret.pendingInit = initialCapacity
	ret.ctx, ret.cancelCtx = context.WithCancel(context.Background())
	ret.new = func() (workspace.VTE, error) {
		return newVTE(ret)
	}

	go debug.CapturePanicReport(func() {
		ret.initCap(initialCapacity)
	})

	// If the underlying scheme can drop and reconnect, watch for
	// invalidation signals and reset the pool so the next Get does
	// not hand out a VTE bound to the dead transport.
	if rs, ok := transport.(workspace.RemoteScheme); ok {
		ch := rs.OnDisconnect()
		if ch != nil {
			go debug.CapturePanicReport(func() {
				ret.watchRemoteScheme(ch)
			})
		}
	}

	return ret
}

// InitialCapacity returns the configured initial capacity.
func (f *Facility) InitialCapacity() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initialCapacity
}

// Capacity returns the current capacity.
func (f *Facility) Capacity() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pool)
}

// WaitForInitialFill blocks until the asynchronous initial warm-up
// kicked off by [New] has settled — i.e. every initCap goroutine
// has either delivered a VTE into the pool or surfaced an error.
// Returns immediately on a closed facility. Production callers do
// not need this: [Get] already waits for at least one warm VTE.
// It exists so tests asserting allocation counts (or any other
// post-warm-up state) can synchronize without poking private
// fields.
func (f *Facility) WaitForInitialFill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.pendingInit > 0 && !f.closed {
		f.cond.Wait()
	}
}

// Get selects an arbitrary vte from the [Facility], removes it from the
// Facility, and returns it to the caller.
func (f *Facility) Get() (workspace.VTE, error) {
	v, dead, err := f.get()
	// Dispose dead VTEs off-lock: DeleteVTE against a dropped
	// transport can block on RPC teardown.
	for _, d := range dead {
		if derr := f.dispose(d); derr != nil {
			f.log(log.WarnLevel, "dispose dead pooled vte: %v", derr)
		}
	}
	return v, err
}

func (f *Facility) get() (v workspace.VTE, dead []workspace.VTE, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Wait for initCap to deliver at least one VTE so that the
	// configured initial capacity is honoured before the first Get
	// falls through to the from-scratch path.
	for len(f.pool) == 0 && f.pendingInit > 0 && !f.closed {
		f.cond.Wait()
	}

	for len(f.pool) != 0 {
		head := f.pool[0]
		f.pool = f.pool[1:]
		// A pooled VTE whose run loop already exited (its process
		// died, or the transport dropped past the one-shot
		// OnDisconnect reset) would open as an empty terminal.
		if head.IsComplete() {
			dead = append(dead, head)
			continue
		}
		// ensure there's always at least one available. The refill
		// is opportunistic: a failure must not cost the caller the
		// warm VTE it already holds.
		if len(f.pool) == 0 {
			newvte, err := f.new()
			if err != nil {
				f.log(log.WarnLevel, "refill vte pool: %v", err)
			} else {
				f.pool = append(f.pool, newvte)
			}
		}
		return head, dead, nil
	}

	v, err = f.new()
	return v, dead, err
}

// Resize can be used to update the width/height of all the vtes in the reservoir.
func (f *Facility) Resize(width, height int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, vte := range f.pool {
		vte.Resize(width, height)
	}
}

// Close closes all vtes associated with this Facility.
func (f *Facility) Close() (ret error) {
	if f.cancelCtx != nil {
		f.cancelCtx()
	}
	f.mu.Lock()
	f.closed = true
	// wake up any Get callers waiting on initCap.
	f.cond.Broadcast()
	return f.reset()
}

func (f *Facility) reset() (ret error) {
	pool := f.pool
	f.pool = nil
	f.mu.Unlock()

	for _, vte := range pool {
		if err := f.dispose(vte); err != nil {
			ret = multierror.Append(ret, err)
		}
	}
	return
}

// dispose destroys a warm terminal nobody claimed.
//
// Closing it would only detach: the emulator lives in the workspace
// server and would survive as a terminal no user ever saw and no
// client can reach. Only DeleteVTE actually reclaims it.
func (f *Facility) dispose(v workspace.VTE) error {
	ctx, cancel := context.WithTimeout(context.Background(), disposeTimeout)
	defer cancel()
	err := f.terminals.DeleteVTE(ctx, v.ID())
	_ = v.Close()
	return err
}

// Reset disposes every VTE currently pooled in this Facility.
// Intended for callers that have detected the underlying transport
// has been invalidated and that handing out cached VTEs would
// surface a confusing failure on the next Read/Write. The next Get
// re-allocates lazily against whatever the executor returns at
// that moment. No-op on a closed Facility.
func (f *Facility) Reset() (ret error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	return f.reset()
}

// watchRemoteScheme drains the warm pool the first time the remote
// scheme reports a transport drop, so the next Get builds against
// whatever the transport resolves to next. OnDisconnect is a
// one-shot signal (see workspace.RemoteScheme) — looping here
// would busy-spin on a permanently closed channel — so the watcher
// exits after the single Reset. Also exits if the facility is
// closed before any drop occurs.
func (f *Facility) watchRemoteScheme(ch <-chan struct{}) {
	select {
	case <-ch:
		if err := f.Reset(); err != nil {
			f.log(log.WarnLevel, "reset on remote scheme invalidation: %v", err)
		}
	case <-f.ctx.Done():
	}
}

func (f *Facility) initCap(initialCapacity int) {
	// pendingInit is normally seeded by New; ensure it is set when
	// initCap is invoked directly (e.g. by the test helper).
	f.mu.Lock()
	if f.pendingInit < initialCapacity {
		f.pendingInit = initialCapacity
	}
	f.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(initialCapacity)
	for range initialCapacity {
		go debug.CapturePanicReport(func() {
			defer wg.Done()
			vte, err := f.new()
			f.mu.Lock()
			f.pendingInit--
			closed := f.closed
			if err == nil && !closed {
				f.pool = append(f.pool, vte)
			}
			f.cond.Broadcast()
			f.mu.Unlock()
			if err != nil {
				f.log(log.WarnLevel, "new vte: %v", err)
				return
			}
			// Do not resurrect a closed pool: dispose the
			// just-created VTE instead of leaking it back in.
			if closed {
				go debug.CapturePanicReport(func() {
					if err := f.dispose(vte); err != nil {
						f.log(log.WarnLevel, "dispose warm vte: %v", err)
					}
				})
				return
			}
		})
	}
	wg.Wait()
}

func (e *Facility) log(level log.Level, msg string, args ...any) {
	if !log.IsLevelEnabled(level) {
		return
	}
	log.WithField(logging.KeyClass, "vtereservoir.Facility").Logf(level, msg, args...)
}
