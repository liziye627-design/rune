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
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/component"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
)

func TestFacility(t *testing.T) {
	t.Parallel()
	t.Run("pre-allocates initial capacity", func(t *testing.T) {
		t.Parallel()
		var called atomic.Int64
		f := newTestFacility(5, func(f *Facility) (workspace.VTE, error) {
			called.Add(1)
			return newTestVte(f), nil
		})
		assert.Equal(t, 5, int(called.Load()))

		_, err := f.Get()
		require.NoError(t, err)

		assert.Equal(t, 5, int(called.Load()))
	})

	t.Run("pre-allocates if pool is empty", func(t *testing.T) {
		t.Parallel()
		var called atomic.Int64
		f := newTestFacility(1, func(f *Facility) (workspace.VTE, error) {
			called.Add(1)
			return newTestVte(f), nil
		})
		assert.Equal(t, 1, int(called.Load()))

		_, err := f.Get()
		require.NoError(t, err)

		assert.Equal(t, 2, int(called.Load()))
	})

	t.Run("refill failure does not cost the caller the warm VTE", func(t *testing.T) {
		t.Parallel()
		var called atomic.Int64
		f := newTestFacility(2, func(f *Facility) (workspace.VTE, error) {
			// Warm-up (calls 1-2) succeeds; the opportunistic
			// refill triggered by draining the pool (call 3) fails.
			if called.Add(1) > 2 {
				return nil, errors.New("stalled transport")
			}
			return newTestVte(f), nil
		})

		_, err := f.Get()
		require.NoError(t, err)

		// Drains the pool: the refill fails, but the caller must
		// still receive the warm VTE it already had.
		v, err := f.Get()
		require.NoError(t, err)
		require.NotNil(t, v)
		assert.Equal(t, 3, int(called.Load()))
	})

	t.Run("Get discards dead pooled vtes", func(t *testing.T) {
		t.Parallel()
		// Regression: a remote transport drop kills the pty run
		// loop of every pooled VTE (Component.IsComplete becomes
		// true) but, past the first one-shot OnDisconnect signal,
		// nothing drains the pool. Get used to hand these dead
		// VTEs to new panes, which then rendered an empty terminal
		// that never opened.
		var mu sync.Mutex
		var created []*recordingVTE
		f := newTestFacility(2, func(f *Facility) (workspace.VTE, error) {
			v := newRecordingVTE()
			mu.Lock()
			defer mu.Unlock()
			created = append(created, v)
			return v, nil
		})

		mu.Lock()
		warm := append([]*recordingVTE(nil), created...)
		mu.Unlock()
		require.Len(t, warm, 2)
		for _, v := range warm {
			v.complete.Store(true)
		}

		got, err := f.Get()
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.False(t, got.IsComplete(),
			"Get must not hand out a VTE whose pty run loop exited")
		for _, v := range warm {
			assert.True(t, v.closed.Load(),
				"dead pooled VTEs must be disposed, not leaked")
		}
	})

	t.Run("facility.Close closes all free vtes", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var created []*testVte
		f := newTestFacility(5, func(f *Facility) (workspace.VTE, error) {
			tvte := newTestVte(f)
			mu.Lock()
			defer mu.Unlock()
			created = append(created, tvte)
			return tvte, nil
		})

		// free and returned
		vte1, err := f.Get()
		require.NoError(t, err)

		vte2, err := f.Get()
		require.NoError(t, err)

		require.NoError(t, vte1.Close())
		require.NoError(t, f.Close())
		require.NoError(t, vte2.Close())

		for _, vte := range created {
			assert.True(t, vte.calledClose)
		}
	})

	t.Run("Resize after new doesn't block waiting for all vtes to be created", func(t *testing.T) {
		t.Parallel()
		var wg sync.WaitGroup
		const capacity = 5
		wg.Add(capacity)
		terms := &funcTerminals{
			create: func(
				ctx context.Context, _ workspace.CreateVTEOptions,
			) (workspace.VTE, error) {
				wg.Done()
				// Never completes on its own: Close cancels it.
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		f := New(terms, nopTerminal{}, vte.DefaultConfig(), capacity)
		t.Cleanup(func() { _ = f.Close() })
		// this would block until every spawn settles if Resize
		// waited on the in-flight creations
		f.Resize(10, 10)
		wg.Wait()
	})

	t.Run("Reset drains the warm pool and lets Get re-allocate", func(t *testing.T) {
		t.Parallel()
		// Track every VTE the factory builds so we can assert that
		// the ones warm at Reset time were closed.
		var mu sync.Mutex
		var all []*testVte
		f := newTestFacility(2, func(f *Facility) (workspace.VTE, error) {
			v := newTestVte(f)
			mu.Lock()
			all = append(all, v)
			mu.Unlock()
			return v, nil
		})
		require.Equal(t, 2, f.Capacity())

		mu.Lock()
		preReset := append([]*testVte(nil), all...)
		mu.Unlock()

		require.NoError(t, f.Reset())

		assert.Equal(t, 0, f.Capacity(),
			"Reset must empty the pool so the next Get re-allocates")
		for _, v := range preReset {
			assert.True(t, v.calledClose,
				"Reset must Close every warm VTE so it is not "+
					"handed out bound to the dead transport")
		}

		v, err := f.Get()
		require.NoError(t, err)
		require.NotNil(t, v)
	})

	t.Run("Reset is a no-op after Close", func(t *testing.T) {
		t.Parallel()
		f := newTestFacility(1, func(f *Facility) (workspace.VTE, error) {
			return newTestVte(f), nil
		})
		require.NoError(t, f.Close())
		require.NoError(t, f.Reset())
	})

	t.Run("remote scheme OnDisconnect drains the pool and exits", func(t *testing.T) {
		t.Parallel()
		// Drive watchRemoteScheme directly with an in-memory fake
		// to keep the test independent of the workspacessh package.
		// Use recordingVTE rather than testVte so Close does not
		// peek at f.pool without locking (which would race against
		// the concurrent Reset below).
		f := newTestFacility(1, func(f *Facility) (workspace.VTE, error) {
			return newRecordingVTE(), nil
		})
		f.ctx, f.cancelCtx = context.WithCancel(context.Background())
		require.Equal(t, 1, f.Capacity())

		rs := newFakeRemoteScheme()
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			f.watchRemoteScheme(rs.OnDisconnect())
		}()

		rs.broadcastDisconnect()

		// One-shot contract: after the single disconnect signal,
		// the watcher resets the pool and exits. Re-arming would
		// busy-loop on a permanently-closed channel.
		select {
		case <-watcherDone:
		case <-time.After(2 * time.Second):
			t.Fatal("watchRemoteScheme must exit after the " +
				"one-shot disconnect signal fires")
		}
		require.Equal(t, 0, f.Capacity(),
			"watchRemoteScheme must Reset the pool when "+
				"OnDisconnect fires")

		require.NoError(t, f.Close())
	})

	t.Run("watchRemoteScheme exits when facility ctx is cancelled", func(t *testing.T) {
		t.Parallel()
		f := newTestFacility(1, func(f *Facility) (workspace.VTE, error) {
			return newRecordingVTE(), nil
		})
		f.ctx, f.cancelCtx = context.WithCancel(context.Background())

		rs := newFakeRemoteScheme()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.watchRemoteScheme(rs.OnDisconnect())
		}()

		// No disconnect: the watcher must still exit when the
		// facility shuts down, otherwise it leaks past Close.
		f.cancelCtx()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("watchRemoteScheme must exit when the facility " +
				"context is cancelled")
		}
		require.NoError(t, f.Close())
	})

	t.Run("Get is served from the pool when warm", func(t *testing.T) {
		t.Parallel()
		var called atomic.Int64
		f := newTestFacility(2, func(f *Facility) (workspace.VTE, error) {
			called.Add(1)
			return newTestVte(f), nil
		})
		// initCap synchronously fires off two news.
		assert.Equal(t, int64(2), called.Load())

		// First Get: pool has 2, take one, pool now has 1 (no
		// refill because len(pool) > 0 after pop).
		_, err := f.Get()
		require.NoError(t, err)
		assert.Equal(t, int64(2), called.Load())

		// Second Get: pool has 1, take it, pool empty so refill -> 1 new call.
		_, err = f.Get()
		require.NoError(t, err)
		assert.Equal(t, int64(3), called.Load())
	})

	t.Run("Get blocks until initCap delivers a pre-warmed VTE", func(t *testing.T) {
		t.Parallel()
		// Use New(...) so that initCap runs asynchronously - this is
		// the exact race that defeats the warm pool in production.
		release := make(chan struct{})
		var newCalls atomic.Int64
		var f *Facility
		terms := &funcTerminals{
			create: func(
				context.Context, workspace.CreateVTEOptions,
			) (workspace.VTE, error) {
				// Block initCap until the test releases it.
				<-release
				newCalls.Add(1)
				return newTestVte(f), nil
			},
		}
		f = New(terms, nopTerminal{}, vte.DefaultConfig(), 2)
		t.Cleanup(func() { _ = f.Close() })

		// Issue a Get before initCap can deliver any VTE. It must not
		// fall through to the from-scratch path; it must wait for the
		// pool to be populated.
		got := make(chan workspace.VTE, 1)
		errCh := make(chan error, 1)
		go func() {
			v, err := f.Get()
			if err != nil {
				errCh <- err
				return
			}
			got <- v
		}()

		// Give the goroutine a chance to enter Get and observe the
		// empty pool. With the bug, Get would immediately call
		// f.new(false) and return.
		select {
		case <-got:
			t.Fatal("Get returned before initCap delivered a VTE")
		case err := <-errCh:
			t.Fatalf("Get returned an unexpected error: %v", err)
		case <-time.After(50 * time.Millisecond):
		}

		// Release initCap so it can populate the pool.
		close(release)

		select {
		case <-got:
		case err := <-errCh:
			t.Fatalf("Get returned an unexpected error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("Get blocked indefinitely waiting for initCap")
		}
	})

	t.Run("initCap does not resurrect a closed pool", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		terms := &fakeTerminals{}
		f := new(Facility)
		f.cond = sync.NewCond(&f.mu)
		f.terminals = terms
		f.initialCapacity = 3
		f.pendingInit = 3
		f.new = func() (workspace.VTE, error) {
			<-release
			return newTestVte(f), nil
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			f.initCap(3)
		}()

		// Close while initCap is mid-flight.
		require.NoError(t, f.Close())
		close(release)
		<-done

		// The pool must remain empty - initCap must not re-populate
		// it after Close.
		assert.Equal(t, 0, f.Capacity())

		// A warm terminal nobody claimed must be deleted, not merely
		// detached: the emulator outlives this process, so detaching
		// would strand a terminal no client can reach.
		require.Eventually(t, func() bool {
			return len(terms.deletedIDs()) == 3
		}, 5*time.Second, 10*time.Millisecond,
			"every VTE created after Close must be deleted")
	})

	// Pins the data race where vteAdapter.Close read f.pool without
	// the facility lock to decide between re-pooling and closing,
	// while the initCap warm-up goroutines appended to the pool
	// under the lock. Get disposes dead pooled VTEs by calling
	// Close off-lock, so the unsynchronized read raced every warm
	// pool append; the race detector flags it.
	t.Run("adapter Close races the warm-up appends safely", func(t *testing.T) {
		t.Parallel()
		// Deliberately not newTestFacility: that waits for the
		// warm-up to settle, which is the very race under test.
		f := newWithFactory(&fakeTerminals{},
			func(f *Facility) (workspace.VTE, error) {
				return newTestVte(f), nil
			}, nopTerminal{}, 8)
		t.Cleanup(func() { _ = f.Close() })

		// Get and dispose adapters while the warm-up is still
		// appending; each Close decides whether the facility can
		// still re-pool it, which must be synchronized with those
		// appends.
		for range 16 {
			v, err := f.Get()
			require.NoError(t, err)
			_ = v.Close()
		}
	})

	// Pins the regression where a stalled remote workspace transport
	// blocked the warm-up's NewPty forever: pendingInit never drained,
	// so Get (called on the host event loop during session restore)
	// waited on the cond indefinitely and froze the UI. With the
	// SpawnTimeout plumbed through New's factory, both the warm-up
	// and Get's fallback allocation fail after the bound.
	t.Run("stalled spawn does not wedge Get indefinitely", func(t *testing.T) {
		t.Parallel()
		terms := &funcTerminals{
			create: func(
				ctx context.Context, _ workspace.CreateVTEOptions,
			) (workspace.VTE, error) {
				// Simulate a wedged transport: the RPC never returns
				// until its context is cancelled.
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		cfg := vte.DefaultConfig()
		cfg.SpawnTimeout = 20 * time.Millisecond
		f := New(terms, nopTerminal{}, cfg, 2)
		t.Cleanup(func() { _ = f.Close() })

		errCh := make(chan error, 1)
		go func() {
			_, err := f.Get()
			errCh <- err
		}()

		select {
		case err := <-errCh:
			require.Error(t, err,
				"Get must surface the spawn failure, not hand out a VTE")
		case <-time.After(10 * time.Second):
			t.Fatal("Get wedged behind a stalled spawn; SpawnTimeout " +
				"must bound the warm-up and the fallback allocation")
		}
	})

	// First-connect provisioning is unbounded (it may install
	// packages on the remote host) and remote scheme calls block
	// until the first connection attempt settles. The spawn budget
	// must not race that wait: a spurious warm-up failure would
	// fail session restore on freshly provisioned workspaces right
	// as they become usable.
	t.Run("spawn budget starts only after the remote transport resolves", func(t *testing.T) {
		t.Parallel()
		var f *Facility
		var createCalls atomic.Int64
		terms := &funcTerminals{
			create: func(
				context.Context, workspace.CreateVTEOptions,
			) (workspace.VTE, error) {
				createCalls.Add(1)
				return newTestVte(f), nil
			},
		}
		gated := &connectGatedTerminal{
			Terminal:     nopTerminal{},
			connected:    make(chan struct{}),
			disconnectCh: make(chan struct{}),
		}
		cfg := vte.DefaultConfig()
		cfg.SpawnTimeout = 20 * time.Millisecond
		f = New(terms, gated, cfg, 1)
		t.Cleanup(func() { _ = f.Close() })

		// Let several spawn-timeout windows elapse while the
		// transport is still "provisioning".
		time.Sleep(5 * cfg.SpawnTimeout)
		assert.EqualValues(t, 0, createCalls.Load(),
			"no spawn RPC may be issued (and no budget consumed) "+
				"while the transport is unresolved")

		close(gated.connected)
		f.WaitForInitialFill()

		v, err := f.Get()
		require.NoError(t, err,
			"Get must succeed once the transport resolves; the "+
				"connect wait must not have failed the warm-up")
		require.NotNil(t, v)
	})
}

func newTestFacility(
	initCap int, newFn func(*Facility) (workspace.VTE, error),
) *Facility {
	// Build the facility via the same internal entrypoint
	// production [New] uses, injecting the test factory so
	// every test exercises the real lifecycle wiring (cond,
	// ctx/cancelCtx, pendingInit, remote-scheme watcher) — no
	// hand-rolled construction or private-field poking.
	f := newWithFactory(&fakeTerminals{}, newFn, nopTerminal{}, initCap)
	// initCap runs asynchronously in production; wait for it to
	// settle so callers observing Capacity / called counters
	// immediately after construction see a fully resolved pool,
	// mirroring the synchronous semantics the previous helper
	// provided.
	f.WaitForInitialFill()
	return f
}

// fakeTerminals stands in for the workspace that owns the pooled
// emulators. Only DeleteVTE is exercised: the factory seam replaces
// CreateVTE so tests can control allocation directly.
type fakeTerminals struct {
	mu      sync.Mutex
	deleted []workspace.VTEID
}

func (f *fakeTerminals) CreateVTE(
	context.Context, workspace.CreateVTEOptions,
) (workspace.VTE, error) {
	return nil, errors.New("unexpected CreateVTE")
}

func (f *fakeTerminals) DeleteVTE(_ context.Context, id workspace.VTEID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeTerminals) deletedIDs() []workspace.VTEID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workspace.VTEID(nil), f.deleted...)
}

// funcTerminals is a Terminals whose CreateVTE is supplied per test.
type funcTerminals struct {
	fakeTerminals
	create func(context.Context, workspace.CreateVTEOptions) (workspace.VTE, error)
}

func (f *funcTerminals) CreateVTE(
	ctx context.Context, opts workspace.CreateVTEOptions,
) (workspace.VTE, error) {
	return f.create(ctx, opts)
}

// nopTerminal satisfies the schemeapi.Terminal slot required by
// [newWithFactory]. The test factory replaces vte.NewHandler so
// these methods are never actually called; the only reason they
// exist is to type-check.
type nopTerminal struct{}

func (nopTerminal) NewPty(context.Context) (workspaceapi.Pty, error) {
	return workspaceapi.Pty{}, nil
}
func (nopTerminal) SetPtySize(workspaceapi.Pty, workspaceapi.PtySize) error { return nil }

var _ workspace.VTE = (*testVte)(nil)

type testVte struct {
	component.String
	initialCmd     string
	f              *Facility
	usedAlt        bool
	clearedPrimary bool
	id             workspace.VTEID

	calledClose bool
}

func newTestVte(f *Facility) *testVte {
	return newTestVteWithConfig(f, "")
}

func newTestVteWithConfig(f *Facility, initialCmd string) *testVte {
	ret := new(testVte)
	ret.initialCmd = initialCmd
	ret.String = component.NewString(initialCmd)
	ret.f = f
	ret.id = workspace.NewVTEID()
	return ret
}

func (t *testVte) ID() workspace.VTEID {
	return t.id
}

func (t *testVte) Handle(ev term.Event) (bool, bool) {
	return false, false
}

func (t *testVte) SeekUp() bool {
	return false
}

func (t *testVte) SeekDown() bool {
	return false
}

func (t *testVte) SeekOffset() int {
	return 0
}

func (t *testVte) MaxSeekOffset() int {
	return 0
}

func (t *testVte) Cursor() (ret term.Coordinates, style term.CursorStyle, show bool) {
	show = true
	ret = term.Coordinates{X: len(t.initialCmd)}
	return
}

func (t *testVte) Selection() (string, bool) {
	return "", false
}

func (t *testVte) Dimensions() (int, int) {
	return 0, 0
}

func (t *testVte) Close() error {
	if t.calledClose {
		return errors.New("called close twice")
	}
	t.calledClose = true
	return nil
}

func (t *testVte) OnFocusChange(inFocus bool) {
}

func (t *testVte) SetDefaultAttributes(attr term.Attributes) {
}

func (t *testVte) Snapshot() (vte.Snapshot, error) {
	return vte.Snapshot{}, nil
}

func (t *testVte) RestoreFromSnapshot(vte.Snapshot) error {
	return nil
}

func (t *testVte) IsComplete() bool {
	return false
}

func (t *testVte) URI() workspaceapi.URI {
	return workspaceapi.URI{}
}

func (t *testVte) Title() string {
	return t.initialCmd
}

func (v *testVte) UsedAlternateBuffer() bool {
	return v.usedAlt
}

func (v *testVte) ClearPrimaryBuffer() bool {
	v.clearedPrimary = true
	return true
}

func (v *testVte) ExitErr() error {
	return nil
}

func (v *testVte) CursorAtScroll() term.Coordinates {
	return term.Coordinates{}
}

// fakeRemoteScheme mirrors workspace.RemoteScheme for tests that
// exercise Facility.watchRemoteScheme without spinning up a real
// SSH transport. The disconnect channel is allocated once at
// construction and closed once by broadcastDisconnect — matching
// the production contract (see workspace.RemoteScheme).
type fakeRemoteScheme struct {
	ch              chan struct{}
	closeOnce       sync.Once
	nilOnDisconnect bool
}

func newFakeRemoteScheme() *fakeRemoteScheme {
	return &fakeRemoteScheme{ch: make(chan struct{})}
}

func (r *fakeRemoteScheme) OnDisconnect() <-chan struct{} {
	if r.nilOnDisconnect {
		return nil
	}
	return r.ch
}

func (r *fakeRemoteScheme) WaitConnected(context.Context) error {
	return nil
}

// connectGatedTerminal simulates a remote scheme whose transport is
// still being established (e.g. first-connect provisioning):
// WaitConnected blocks until the test closes connected. The embedded
// Terminal (a NopScheme) serves the pty RPCs once resolved.
type connectGatedTerminal struct {
	schemeapi.Terminal
	connected    chan struct{}
	disconnectCh chan struct{}
}

func (g *connectGatedTerminal) OnDisconnect() <-chan struct{} {
	return g.disconnectCh
}

func (g *connectGatedTerminal) WaitConnected(ctx context.Context) error {
	select {
	case <-g.connected:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *fakeRemoteScheme) broadcastDisconnect() {
	r.closeOnce.Do(func() { close(r.ch) })
}

// recordingVTE is a minimal VTE used by the remote-scheme tests.
// Unlike testVte it does not peek at Facility internals from
// Close, so it is safe to drive across goroutines while another
// goroutine mutates the pool.
type recordingVTE struct {
	component.String
	closed   atomic.Bool
	complete atomic.Bool
	id       workspace.VTEID
}

func newRecordingVTE() *recordingVTE {
	r := new(recordingVTE)
	r.String = component.NewString("")
	r.id = workspace.NewVTEID()
	return r
}

func (r *recordingVTE) ID() workspace.VTEID            { return r.id }
func (r *recordingVTE) Handle(term.Event) (bool, bool) { return false, false }
func (r *recordingVTE) SeekUp() bool                   { return false }
func (r *recordingVTE) SeekDown() bool                 { return false }
func (r *recordingVTE) SeekOffset() int                { return 0 }
func (r *recordingVTE) MaxSeekOffset() int             { return 0 }
func (r *recordingVTE) Cursor() (term.Coordinates, term.CursorStyle, bool) {
	return term.Coordinates{}, term.CursorStyleDefault, false
}
func (r *recordingVTE) Selection() (string, bool)              { return "", false }
func (r *recordingVTE) Dimensions() (int, int)                 { return 0, 0 }
func (r *recordingVTE) OnFocusChange(bool)                     {}
func (r *recordingVTE) SetDefaultAttributes(term.Attributes)   {}
func (r *recordingVTE) Snapshot() (vte.Snapshot, error)        { return vte.Snapshot{}, nil }
func (r *recordingVTE) RestoreFromSnapshot(vte.Snapshot) error { return nil }
func (r *recordingVTE) IsComplete() bool                       { return r.complete.Load() }
func (r *recordingVTE) URI() workspaceapi.URI                  { return workspaceapi.URI{} }
func (r *recordingVTE) Title() string                          { return "" }
func (r *recordingVTE) UsedAlternateBuffer() bool              { return false }
func (r *recordingVTE) ClearPrimaryBuffer() bool               { return true }
func (r *recordingVTE) ExitErr() error                         { return nil }
func (r *recordingVTE) CursorAtScroll() term.Coordinates       { return term.Coordinates{} }
func (r *recordingVTE) Close() error                           { r.closed.Store(true); return nil }
