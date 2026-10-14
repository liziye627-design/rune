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

#import "RuneStatus.h"

NS_ASSUME_NONNULL_BEGIN

/// What the Dock menu's items do. Every action is sent to the menu's
/// target with the item as sender; the item's representedObject names
/// what it is about.
@protocol RuneDockMenuActions <NSObject>
/// representedObject is the peer's hostname.
- (void)openPeer:(id)sender;
/// representedObject is the workspace root.
- (void)openWorkspace:(id)sender;
/// representedObject is the URL to sign in at.
- (void)signIn:(id)sender;
- (void)connectNetwork:(id)sender;
- (void)disconnectNetwork:(id)sender;
/// representedObject is the number of terminals still running.
- (void)stopServer:(id)sender;
- (void)openServerLog:(id)sender;
@end

/// The dot before a row: the state of the network, or of a peer.
typedef NS_ENUM(NSInteger, RuneDot) {
  RuneDotNone,
  RuneDotGreen,
  RuneDotGray,
  RuneDotAmber,
};

/// Builds the Dock menu for a published status.
@interface RuneDockMenu : NSObject

- (instancetype)initWithTarget:(id<RuneDockMenuActions>)target;

/// The menu for status, or for no running server when status is nil.
- (NSMenu *)menuForStatus:(nullable RuneStatus *)status;

/// The image of dot, drawn rather than shipped so the plug-in has no
/// resources to load from inside the Dock. nil for RuneDotNone. Rune's
/// own Dock menu (internal/term/gui/docktile) draws the same dots and
/// cannot link this code, so the two change together.
+ (nullable NSImage *)imageForDot:(RuneDot)dot;

@end

NS_ASSUME_NONNULL_END
