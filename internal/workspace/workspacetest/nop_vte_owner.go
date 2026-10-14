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

package workspacetest

import (
	"context"

	"unstable.build/rune/internal/workspace"
)

// NopVTEOwner satisfies workspace.VTEOwner for test workspaces that own
// no terminals, the way workspace.NewSchemeWorkspace does for a scheme
// without them.
type NopVTEOwner struct{}

var _ workspace.VTEOwner = NopVTEOwner{}

// CreateVTE satisfies workspace.VTEOwner.
func (NopVTEOwner) CreateVTE(
	context.Context, workspace.CreateVTEOptions,
) (workspace.VTE, error) {
	return nil, workspace.ErrVTEUnsupported
}

// ListVTEs satisfies workspace.VTEOwner.
func (NopVTEOwner) ListVTEs(context.Context) ([]workspace.VTEInfo, error) {
	return nil, workspace.ErrVTEUnsupported
}

// AttachVTE satisfies workspace.VTEOwner.
func (NopVTEOwner) AttachVTE(
	context.Context, workspace.VTEID,
) (workspace.VTE, error) {
	return nil, workspace.ErrVTEUnsupported
}

// DeleteVTE satisfies workspace.VTEOwner.
func (NopVTEOwner) DeleteVTE(context.Context, workspace.VTEID) error {
	return workspace.ErrVTEUnsupported
}
