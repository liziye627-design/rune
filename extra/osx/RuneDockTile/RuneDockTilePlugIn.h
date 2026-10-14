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

#import <Cocoa/Cocoa.h>

#import "RuneDockMenu.h"

NS_ASSUME_NONNULL_BEGIN

/// The Dock tile of Rune while Rune is not running. The Dock loads it
/// from the app bundle's NSDockTilePlugIn and it shows what the
/// background workspace server publishes: how many terminals it keeps
/// alive, as the badge, and the Rune network, in the Dock menu. While
/// Rune runs, Rune's own delegate serves the menu instead.
///
/// It runs inside the Dock, so it never blocks the main thread and
/// never touches the server but through its control socket.
@interface RuneDockTilePlugIn : NSObject <NSDockTilePlugIn, RuneDockMenuActions>
@end

NS_ASSUME_NONNULL_END
