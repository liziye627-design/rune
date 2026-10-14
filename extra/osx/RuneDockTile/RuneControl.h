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

#import <Foundation/Foundation.h>

NS_ASSUME_NONNULL_BEGIN

extern NSErrorDomain const RuneControlErrorDomain;

/// The commands the workspace server takes over its control socket.
extern NSString *const RuneControlCommandNetworkUp;
extern NSString *const RuneControlCommandNetworkDown;
extern NSString *const RuneControlCommandStop;

/// A client of the workspace server's control socket: one command
/// line per connection, answered with "ok" or "err <message>". See
/// internal/workspace/workspaced/control.go.
@interface RuneControl : NSObject

/// The control socket of the server of the default data directory.
+ (NSString *)socketPath;

/// Sends command and waits up to timeout for the answer. Returns YES
/// on "ok". On "err <message>", or when the server cannot be reached
/// or does not answer in time, returns NO with the reason in error.
/// It blocks, so it is not for the main thread.
+ (BOOL)send:(NSString *)command
     timeout:(NSTimeInterval)timeout
       error:(NSError *_Nullable *_Nullable)error;

@end

NS_ASSUME_NONNULL_END
