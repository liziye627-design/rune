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

import "unstable.build/rune/internal/workspace"

const terminalSnapshotVersion = 1

// Snapshot is a durable representation of a terminal's rendered state.
// It belongs to the scheme that owns the emulator, so it is defined
// where the terminal lifecycle is: see [workspace.VTESnapshot].
type Snapshot = workspace.VTESnapshot

// ScreenSnapshot is the rendered state of one VTE screen buffer.
// See [workspace.VTEScreenSnapshot].
type ScreenSnapshot = workspace.VTEScreenSnapshot
