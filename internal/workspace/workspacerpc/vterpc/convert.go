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

package vterpc

import (
	"time"

	"unstable.build/rune/internal/workspace"
)

// FromModel fills i with info.
func (x *Info) FromModel(info workspace.VTEInfo) {
	x.Id = string(info.ID)
	x.Title = info.Title
	x.CommandAndArgs = info.CommandAndArgs
	x.Width = int32(info.Width)
	x.Height = int32(info.Height)
	x.Attachments = int32(info.Attachments)
	x.Complete = info.Complete
	x.CreatedAtUnixNano = info.CreatedAt.UnixNano()
}

// ToModel converts x to its model representation.
func (x *Info) ToModel() workspace.VTEInfo {
	return workspace.VTEInfo{
		ID:             workspace.VTEID(x.GetId()),
		Title:          x.GetTitle(),
		CommandAndArgs: x.GetCommandAndArgs(),
		Width:          int(x.GetWidth()),
		Height:         int(x.GetHeight()),
		Attachments:    int(x.GetAttachments()),
		Complete:       x.GetComplete(),
		CreatedAt:      time.Unix(0, x.GetCreatedAtUnixNano()),
	}
}
