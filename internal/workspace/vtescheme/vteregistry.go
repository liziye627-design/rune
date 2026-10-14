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
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ernestrc/go-multierror"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
)

// VTEFactory builds a canonical emulator for opts whose program runs in
// the workspace rooted at root. It is supplied by the process hosting
// the workspace server because vte.Handler needs browser-facing
// collaborators the transport knows nothing about.
//
// cellPixelSize reports the size of a cell in pixels in force, which
// the attached clients decide. The emulator needs it for
// [vte.Config.CellPixelSize]: it is what graphics are placed and pixel
// queries answered with, and what the pty's pixel size is.
type VTEFactory func(
	root string, opts workspace.CreateVTEOptions,
	cellPixelSize func() (width, height int),
) (*vte.Handler, error)

// VTERegistry owns the canonical terminal emulators of every workspace
// a server hosts: their handlers, ptys, processes, rendered grids,
// scrollback, titles and stable identities. Clients hold attachments,
// never the emulator itself, so a terminal outlives the client that
// opened it and only [VTERegistry.DeleteVTE] terminates it.
//
// Every emulator belongs to the workspace it was created in, and each
// workspace only sees its own: a restored session must never adopt a
// terminal another workspace opened.
type VTERegistry struct {
	newVTE VTEFactory
	// onChange is told that what Snapshot reports changed. It runs on
	// whichever goroutine made the change, so it must return promptly.
	onChange func()

	mu      sync.Mutex
	entries map[workspace.VTEID]*canonicalVTE
}

// NewVTERegistry allocates storage for a new VTERegistry and
// initializes it with newVTE. onChange, when not nil, is called after
// a terminal is created, terminated, attached to or detached from, and
// when its process exits: every change to what [VTERegistry.Snapshot]
// reports. It is called on whichever goroutine made the change and
// may run concurrently with itself, so it must return promptly and
// must coalesce on its own.
func NewVTERegistry(newVTE VTEFactory, onChange func()) *VTERegistry {
	if newVTE == nil {
		panic("vtescheme: VTEFactory is required")
	}
	return &VTERegistry{
		newVTE:   newVTE,
		onChange: onChange,
		entries:  make(map[workspace.VTEID]*canonicalVTE),
	}
}

func (r *VTERegistry) changed() {
	if r.onChange != nil {
		r.onChange()
	}
}

// CreateVTE creates an emulator in the workspace rooted at root.
func (r *VTERegistry) CreateVTE(
	_ context.Context, root string, opts workspace.CreateVTEOptions,
) (workspace.VTE, error) {
	a, err := r.create(root, opts, cellSize{})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// create creates an emulator whose cells start at cell, the size they
// have on the display of the client creating it.
func (r *VTERegistry) create(
	root string, opts workspace.CreateVTEOptions, cell cellSize,
) (*vteAttachment, error) {
	c := &canonicalVTE{
		id:          workspace.NewVTEID(),
		root:        root,
		opts:        opts,
		createdAt:   time.Now(),
		width:       opts.Width,
		height:      opts.Height,
		attachments: make(map[*vteAttachment]struct{}),
		hub:         newTeeHub(),
		changed:     r.changed,
	}
	c.cell.store(cell)
	h, err := r.newVTE(root, opts, c.cell.pixels)
	if err != nil {
		return nil, err
	}
	c.h = h
	// The process may already be writing, but no attachment exists
	// yet: each one starts from a snapshot taken after this, so output
	// that predates the tees is in that snapshot rather than lost.
	h.Component().TeeStream(func(batch []byte) {
		c.hub.broadcast(&vtewire.Output{Data: batch})
	})
	h.Component().TeeGraphicsMedia(vtewire.MaxGraphicsMediumLen,
		func(data []byte, ok bool) {
			c.hub.broadcast(&vtewire.GraphicsMedium{OK: ok, Data: data})
		})
	if opts.Width > 0 && opts.Height > 0 {
		h.Resize(opts.Width, opts.Height)
	}
	// Attached before it is registered: nothing can terminate it yet.
	a, _ := c.attach()
	r.mu.Lock()
	r.entries[c.id] = c
	r.mu.Unlock()
	// The process ending is the one change nobody asks the registry
	// for, so it is observed from the emulator's own stream.
	go debug.CapturePanicReport(func() {
		<-h.Component().Done()
		r.changed()
	})
	r.changed()
	return a, nil
}

// AttachVTE attaches to an emulator of the workspace rooted at root.
func (r *VTERegistry) AttachVTE(
	_ context.Context, root string, id workspace.VTEID,
) (workspace.VTE, error) {
	c, ok := r.lookup(root, id)
	if !ok {
		return nil, workspace.ErrVTENotFound
	}
	a, ok := c.attach()
	if !ok {
		return nil, workspace.ErrVTENotFound
	}
	r.changed()
	return a, nil
}

// ListVTEs lists the emulators of the workspace rooted at root.
func (r *VTERegistry) ListVTEs(
	_ context.Context, root string,
) ([]workspace.VTEInfo, error) {
	entries := r.snapshotEntries()
	ret := make([]workspace.VTEInfo, 0, len(entries))
	for _, c := range entries {
		if c.root == root {
			ret = append(ret, c.info())
		}
	}
	return ret, nil
}

// DeleteVTE terminates an emulator of the workspace rooted at root.
func (r *VTERegistry) DeleteVTE(
	_ context.Context, root string, id workspace.VTEID,
) error {
	r.mu.Lock()
	c, ok := r.entries[id]
	ok = ok && c.root == root
	if ok {
		delete(r.entries, id)
	}
	r.mu.Unlock()
	if !ok {
		return workspace.ErrVTENotFound
	}
	err := c.destroy()
	r.changed()
	return err
}

// Snapshot reports every emulator the registry owns, keyed by the root
// of the workspace it belongs to and ordered by creation.
func (r *VTERegistry) Snapshot() map[string][]workspace.VTEInfo {
	entries := r.snapshotEntries()
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].createdAt.Equal(entries[j].createdAt) {
			return entries[i].createdAt.Before(entries[j].createdAt)
		}
		return entries[i].id < entries[j].id
	})
	ret := make(map[string][]workspace.VTEInfo)
	for _, c := range entries {
		ret[c.root] = append(ret[c.root], c.info())
	}
	return ret
}

func (r *VTERegistry) lookup(
	root string, id workspace.VTEID,
) (*canonicalVTE, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.entries[id]
	if !ok || c.root != root {
		return nil, false
	}
	return c, true
}

// Close destroys every canonical emulator. It belongs to workspace
// server shutdown: detaching a client, closing a Rune window or
// exiting Rune must not reach it.
func (r *VTERegistry) Close() error {
	r.mu.Lock()
	entries := make([]*canonicalVTE, 0, len(r.entries))
	for id, c := range r.entries {
		entries = append(entries, c)
		delete(r.entries, id)
	}
	r.mu.Unlock()

	var errs *multierror.Error
	for _, c := range entries {
		errs = multierror.Append(errs, c.destroy())
	}
	if len(entries) > 0 {
		r.changed()
	}
	return errs.ErrorOrNil()
}

func (r *VTERegistry) snapshotEntries() []*canonicalVTE {
	r.mu.Lock()
	defer r.mu.Unlock()
	ret := make([]*canonicalVTE, 0, len(r.entries))
	for _, c := range r.entries {
		ret = append(ret, c)
	}
	return ret
}

// canonicalVTE is the server-side emulator plus the state shared by
// every client attached to it. Its mutex serializes both registry
// bookkeeping and emulator calls, which is what gives every client one
// canonical interpretation of the terminal.
type canonicalVTE struct {
	id        workspace.VTEID
	root      string
	h         *vte.Handler
	opts      workspace.CreateVTEOptions
	createdAt time.Time
	// hub fans the emulator's raw pty stream out to wire clients.
	hub *teeHub
	// changed reports a detach to the registry's observer; every other
	// change is reported by the registry operation that made it.
	changed func()
	// cell is the size of a cell in pixels in force. It changes under
	// mu, but the emulator reads it from its parse goroutine, which a
	// resize holding mu waits on.
	cell atomicCellSize

	mu          sync.Mutex
	attachments map[*vteAttachment]struct{}
	// resizeOwner holds resize authority. Only its requests reach the
	// canonical pty; the size is retained while it is nil.
	resizeOwner   *vteAttachment
	focusSeq      uint64
	attachSeq     uint64
	width, height int
	destroyed     bool
}

// attach reports false when the emulator was terminated, which a
// caller that found it in the registry may yet see.
func (c *canonicalVTE) attach() (*vteAttachment, bool) {
	a := &vteAttachment{c: c}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed {
		return nil, false
	}
	c.attachSeq++
	a.seq = c.attachSeq
	c.attachments[a] = struct{}{}
	if c.resizeOwner == nil {
		c.resizeOwner = a
	}
	return a, true
}

func (c *canonicalVTE) detach(a *vteAttachment) {
	c.mu.Lock()
	if _, ok := c.attachments[a]; !ok {
		c.mu.Unlock()
		return
	}
	delete(c.attachments, a)
	if c.resizeOwner == a {
		c.resizeOwner = c.nextResizeOwnerLocked()
		// A client only asks for a size when its own window changes,
		// so the heir would otherwise render at the departed client's
		// size until the user resized something.
		if heir := c.resizeOwner; heir != nil {
			c.followLocked(heir)
		}
	}
	c.mu.Unlock()
	c.changed()
}

// nextResizeOwnerLocked picks the most recently focused remaining
// attachment, falling back to the oldest one.
func (c *canonicalVTE) nextResizeOwnerLocked() *vteAttachment {
	var ret *vteAttachment
	for a := range c.attachments {
		if ret == nil || a.focusedAt > ret.focusedAt ||
			(a.focusedAt == ret.focusedAt && a.seq < ret.seq) {
			ret = a
		}
	}
	return ret
}

func (c *canonicalVTE) info() workspace.VTEInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	comp := c.h.Component()
	return workspace.VTEInfo{
		ID:             c.id,
		Title:          comp.Title(),
		CommandAndArgs: c.opts.CommandAndArgs,
		Width:          c.width,
		Height:         c.height,
		Attachments:    len(c.attachments),
		Complete:       comp.IsComplete(),
		CreatedAt:      c.createdAt,
	}
}

func (c *canonicalVTE) destroy() error {
	c.mu.Lock()
	if c.destroyed {
		c.mu.Unlock()
		return nil
	}
	c.destroyed = true
	attached := make([]*vteAttachment, 0, len(c.attachments))
	for a := range c.attachments {
		attached = append(attached, a)
	}
	clear(c.attachments)
	c.resizeOwner = nil
	err := c.h.Close()
	c.mu.Unlock()

	for _, a := range attached {
		a.detached.Store(true)
	}
	return err
}

// resize applies a client's requested size when that client holds
// authority and reports the size actually in force. A request without
// a cell size keeps the one in force: a client that cannot measure a
// cell has no say in it.
//
// The emulator is resized while its feed is held still so every wire
// client sees the resize at the same point in the byte stream it
// happened for the canonical emulator, which is what makes the
// replicas reflow identically.
func (c *canonicalVTE) resize(a *vteAttachment, req vtewire.Resize) vtewire.Resize {
	c.mu.Lock()
	defer c.mu.Unlock()
	a.width, a.height = req.Width, req.Height
	if cell := cellSizeOf(req.CellPixelWidth, req.CellPixelHeight); cell != (cellSize{}) {
		a.cell = cell
	}
	if c.resizeOwner == a {
		c.followLocked(a)
	}
	return c.geometryLocked()
}

// followLocked gives the pty the size a asked for, which is how the
// holder of resize authority is followed. Whatever a has not named
// stays in force.
func (c *canonicalVTE) followLocked(a *vteAttachment) {
	if a.width <= 0 || a.height <= 0 {
		return
	}
	cell := a.cell
	if cell == (cellSize{}) {
		cell = c.cell.load()
	}
	want := vtewire.Resize{
		Width:           a.width,
		Height:          a.height,
		CellPixelWidth:  cell.width,
		CellPixelHeight: cell.height,
	}
	if want != c.geometryLocked() {
		c.applyResizeLocked(want)
	}
}

func (c *canonicalVTE) applyResizeLocked(g vtewire.Resize) {
	c.width, c.height = g.Width, g.Height
	c.h.Component().WithFeedHeld(func() {
		// The cell changes between two feeds, as the grid does, so
		// every replica places an image at the size this emulator did.
		c.cell.store(cellSize{width: g.CellPixelWidth, height: g.CellPixelHeight})
		c.h.Resize(g.Width, g.Height)
		c.hub.broadcast(&g)
	})
}

// geometry reports the size currently in force for every client.
func (c *canonicalVTE) geometry() vtewire.Resize {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.geometryLocked()
}

func (c *canonicalVTE) geometryLocked() vtewire.Resize {
	cell := c.cell.load()
	return vtewire.Resize{
		Width:           c.width,
		Height:          c.height,
		CellPixelWidth:  cell.width,
		CellPixelHeight: cell.height,
	}
}

func (c *canonicalVTE) onFocusChange(a *vteAttachment, inFocus bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if inFocus {
		c.focusSeq++
		a.focusedAt = c.focusSeq
		c.resizeOwner = a
		c.followLocked(a)
	}
	c.h.OnFocusChange(inFocus)
}

// maxCellPixels bounds the size of a cell in pixels a client may ask
// for. No display has cells anywhere near this large.
const maxCellPixels = 1024

// cellSize is the size of a cell in pixels. The zero value is none: a
// display that cannot draw images, for which graphics are off.
type cellSize struct{ width, height int }

// cellSizeOf is the cell size a peer named, which is none unless both
// of its dimensions are.
func cellSizeOf(width, height int) cellSize {
	if width <= 0 || height <= 0 {
		return cellSize{}
	}
	return cellSize{
		width:  min(width, maxCellPixels),
		height: min(height, maxCellPixels),
	}
}

// atomicCellSize is a cell size an emulator reads from its parse
// goroutine while whoever changes it may be waiting on that
// goroutine's feed.
type atomicCellSize struct{ v atomic.Uint64 }

func (a *atomicCellSize) load() cellSize {
	v := a.v.Load()
	return cellSize{width: int(v >> 32), height: int(uint32(v))}
}

func (a *atomicCellSize) store(c cellSize) {
	a.v.Store(uint64(uint32(c.width))<<32 | uint64(uint32(c.height)))
}

// pixels satisfies [vte.Config.CellPixelSize].
func (a *atomicCellSize) pixels() (width, height int) {
	c := a.load()
	return c.width, c.height
}

// vteAttachment is one client's view of a canonical emulator. Close
// detaches this client only.
type vteAttachment struct {
	c        *canonicalVTE
	detached atomic.Bool

	// guarded by c.mu
	seq       uint64
	focusedAt uint64
	// width, height and cell are what this client last asked for,
	// which the pty takes whenever this client holds resize authority.
	width, height int
	cell          cellSize
}

var _ workspace.VTE = (*vteAttachment)(nil)

// ID satisfies workspace.VTE.
func (a *vteAttachment) ID() workspace.VTEID { return a.c.id }

// Close detaches this client. The canonical emulator, its pty and its
// process keep running; only VTERegistry.DeleteVTE terminates them.
func (a *vteAttachment) Close() error {
	if a.detached.Swap(true) {
		return nil
	}
	a.c.detach(a)
	return nil
}

// Resize satisfies tui.Component. The request only reaches the
// canonical pty when this attachment holds resize authority.
func (a *vteAttachment) Resize(width, height int) {
	a.c.resize(a, vtewire.Resize{Width: width, Height: height})
}

// Draw satisfies tui.Component.
func (a *vteAttachment) Draw(w term.Writer) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	a.c.h.Draw(w)
}

// Handle satisfies tui.Handler.
func (a *vteAttachment) Handle(ev term.Event) (exit, handled bool) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.Handle(ev)
}

// Cursor satisfies tui.Handler.
func (a *vteAttachment) Cursor() (term.Coordinates, term.CursorStyle, bool) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.Cursor()
}

// Selection satisfies tui.Handler.
func (a *vteAttachment) Selection() (string, bool) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.Selection()
}

// SeekUp satisfies component.Scrollable.
func (a *vteAttachment) SeekUp() bool {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.SeekUp()
}

// SeekDown satisfies component.Scrollable.
func (a *vteAttachment) SeekDown() bool {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.SeekDown()
}

// SeekOffset satisfies component.Scrollable.
func (a *vteAttachment) SeekOffset() int {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.SeekOffset()
}

// MaxSeekOffset satisfies component.Scrollable.
func (a *vteAttachment) MaxSeekOffset() int {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.MaxSeekOffset()
}

// OnFocusChange satisfies workspace.VTE. A focused attachment takes
// over resize authority.
func (a *vteAttachment) OnFocusChange(inFocus bool) {
	a.c.onFocusChange(a, inFocus)
}

// SetDefaultAttributes satisfies workspace.VTE.
func (a *vteAttachment) SetDefaultAttributes(attr term.Attributes) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	a.c.h.SetDefaultAttributes(attr)
}

// Snapshot satisfies workspace.VTE.
func (a *vteAttachment) Snapshot() (workspace.VTESnapshot, error) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.Snapshot()
}

// RestoreFromSnapshot satisfies workspace.VTE.
func (a *vteAttachment) RestoreFromSnapshot(s workspace.VTESnapshot) error {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.RestoreFromSnapshot(s)
}

// IsComplete satisfies workspace.VTE.
func (a *vteAttachment) IsComplete() bool {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.Component().IsComplete()
}

// URI satisfies workspace.VTE.
func (a *vteAttachment) URI() workspaceapi.URI {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.Component().URI()
}

// Title satisfies workspace.VTE.
func (a *vteAttachment) Title() string {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.Component().Title()
}

// UsedAlternateBuffer satisfies workspace.VTE.
func (a *vteAttachment) UsedAlternateBuffer() bool {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.Component().UsedAlternateBuffer()
}

// ClearPrimaryBuffer satisfies workspace.VTE.
func (a *vteAttachment) ClearPrimaryBuffer() bool {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.ClearPrimaryBuffer()
}

// ExitErr satisfies workspace.VTE.
func (a *vteAttachment) ExitErr() error {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.ExitErr()
}

// CursorAtScroll satisfies workspace.VTE.
func (a *vteAttachment) CursorAtScroll() term.Coordinates {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.h.CursorAtScroll()
}
