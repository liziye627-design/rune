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

package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/component"
	"github.com/unstablebuild/rune-go-sdk/term"
)

// VTEID is the stable identity of a terminal emulator owned by a
// workspace. It deliberately outlives transports, pty file descriptors
// and process ids so a client can reattach to the same emulator after
// its process exits or its transport reconnects.
type VTEID string

// NewVTEID returns a fresh identity for a terminal emulator.
func NewVTEID() VTEID {
	var b [16]byte
	// rand.Read never fails; see crypto/rand.Read documentation.
	_, _ = rand.Read(b[:])
	return VTEID(hex.EncodeToString(b[:]))
}

// ErrVTENotFound is returned by AttachVTE and DeleteVTE when the
// workspace was reached and confirmed that the requested emulator does
// not exist.
//
// It is the only error that authorizes recreating a terminal from a
// snapshot: any other failure (transport loss, authentication, timeout,
// cancellation, protocol incompatibility) leaves the emulator's
// existence unknown, and a replacement would duplicate a live process.
var ErrVTENotFound = errors.New("workspace: vte not found")

// ErrVTEUnsupported is returned by workspaces whose scheme does not own
// terminal emulators.
var ErrVTEUnsupported = errors.New("workspace: scheme does not own terminals")

// VTEInfo is the client-independent description of a terminal emulator.
type VTEInfo struct {
	ID    VTEID
	Title string
	// CommandAndArgs is the program the emulator was created with,
	// empty when it was created with the workspace's default shell.
	CommandAndArgs []string
	Width          int
	Height         int
	// Attachments is the number of clients currently attached.
	Attachments int
	// Complete reports that the emulator's process has exited. The
	// emulator remains listable and attachable so its final output can
	// still be read.
	Complete  bool
	CreatedAt time.Time
}

// CreateVTEOptions describes a terminal emulator to create. It is the
// serializable subset of an emulator's configuration: client-local
// concerns such as clipboards, event scheduling and bell handling stay
// with the attaching client.
type CreateVTEOptions struct {
	// CommandAndArgs is the program to run. Empty runs the workspace's
	// default shell.
	CommandAndArgs []string
	// Width and Height are the initial pty dimensions.
	Width  int
	Height int
	// MaxLines bounds the scrollback.
	MaxLines int
}

// VTESnapshot is the durable rendered state of a terminal emulator.
type VTESnapshot struct {
	Schema  int
	Version uint64
	Title   string
	Width   int
	Height  int

	ScrollOffset term.Coordinates
	Primary      VTEScreenSnapshot
	Alternate    VTEScreenSnapshot
}

// ActiveCells returns a copy of the cells of the active screen.
func (s VTESnapshot) ActiveCells() [][]term.Cell {
	return term.CloneCells(s.Active().Cells)
}

// Active returns the screen the emulator was last rendering into.
func (s VTESnapshot) Active() VTEScreenSnapshot {
	if len(s.Alternate.Cells) != 0 {
		return s.Alternate
	}
	return s.Primary
}

// VTEScreenSnapshot is the rendered state of one emulator screen buffer.
type VTEScreenSnapshot struct {
	Cells  [][]term.Cell
	Cursor term.Coordinates
}

// VTE is a client's attachment to a terminal emulator owned by a
// workspace.
//
// A VTE value represents the attachment, not the terminal: Close
// detaches this client and leaves the emulator, its pty and its process
// running. Only [VTEOwner.DeleteVTE] terminates them.
type VTE interface {
	browserapi.Handler
	component.Scrollable

	// ID is the stable identity of the emulator this value is
	// attached to.
	ID() VTEID
	OnFocusChange(bool)
	SetDefaultAttributes(attr term.Attributes)
	Snapshot() (VTESnapshot, error)
	RestoreFromSnapshot(VTESnapshot) error
	IsComplete() bool
	URI() workspaceapi.URI
	Title() string
	UsedAlternateBuffer() bool
	ClearPrimaryBuffer() bool
	// ExitErr returns the error the process exited with: nil while it
	// runs, when it exited cleanly, or when its status is unknown.
	ExitErr() error
	// CursorAtScroll returns the cursor's position in the rows
	// Snapshot captures, scrollback included.
	CursorAtScroll() term.Coordinates
}

// VTEOwner is implemented by schemes that own terminal emulators.
//
// Emulators are addressed by a stable [VTEID] rather than by a pty
// descriptor so a scheme is free to keep them alive across the
// transports and processes its clients use. Terminal emulators are a
// host concern: the extension-facing scheme contract carries files and
// processes only, so this capability is discovered on a scheme the way
// [RemoteScheme] and [InstallDataDirProvider] are.
type VTEOwner interface {
	// CreateVTE creates a terminal emulator and returns the calling
	// client's attachment to it.
	CreateVTE(context.Context, CreateVTEOptions) (VTE, error)
	// ListVTEs reports every emulator the scheme owns, attached or not.
	ListVTEs(context.Context) ([]VTEInfo, error)
	// AttachVTE returns a new attachment to an existing emulator, or
	// [ErrVTENotFound] when the scheme is reachable and the emulator is
	// gone.
	AttachVTE(context.Context, VTEID) (VTE, error)
	// DeleteVTE destroys an emulator: it stops the pty and process, so
	// every attachment sees the terminal exit, and forgets the
	// emulator. It is the only operation that terminates a terminal.
	DeleteVTE(context.Context, VTEID) error
}

// AsVTEOwner is scheme's terminals, for a decorator forwarding
// [VTEOwner] to the scheme it wraps: it fails with [ErrVTEUnsupported]
// when scheme owns none.
func AsVTEOwner(scheme any) (VTEOwner, error) {
	if o, ok := scheme.(VTEOwner); ok {
		return o, nil
	}
	return nil, ErrVTEUnsupported
}
