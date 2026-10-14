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

/// One other machine on the Rune network.
@interface RuneStatusPeer : NSObject
@property (nonatomic, copy, readonly) NSString *hostname;
@property (nonatomic, copy, readonly) NSString *dnsName;
@property (nonatomic, copy, readonly) NSString *os;
@property (nonatomic, readonly) BOOL online;
@end

/// One terminal the workspace server owns.
@interface RuneStatusTerminal : NSObject
@property (nonatomic, copy, readonly) NSString *identifier;
@property (nonatomic, copy, readonly) NSString *title;
@property (nonatomic, copy, readonly) NSArray<NSString *> *commandAndArgs;
@property (nonatomic, readonly) NSInteger attachments;
@property (nonatomic, readonly) BOOL complete;
/// What the menu calls the terminal: its title, or its program when
/// it has none yet, or "shell" for a plain shell.
@property (nonatomic, copy, readonly) NSString *displayName;
@end

/// One workspace whose terminals the server owns.
@interface RuneStatusWorkspace : NSObject
@property (nonatomic, copy, readonly) NSString *root;
@property (nonatomic, copy, readonly) NSArray<RuneStatusTerminal *> *terminals;
@end

/// How the server's membership in the Rune network is shown.
typedef NS_ENUM(NSInteger, RuneNetworkState) {
  /// The node is off, or the server hosts none.
  RuneNetworkStateDown,
  /// The node is Running.
  RuneNetworkStateUp,
  /// The node waits for the user to sign in.
  RuneNetworkStateSignIn,
  /// The node could not join, or is between states.
  RuneNetworkStateError,
};

/// What the workspace server of the default data directory publishes
/// about itself. The server rewrites the file whenever something in it
/// changes and removes it when it stops; see
/// internal/workspace/workspaced/status.go for the schema.
@interface RuneStatus : NSObject

/// The data directory a Rune started without --datadir uses.
+ (NSString *)dataDirectory;
/// The directory the status and the server's sockets live in.
+ (NSString *)serversDirectory;
+ (NSString *)statusPath;
+ (NSString *)serverLogPath;

/// Reads the published status. Returns nil when no server publishes
/// one, the file cannot be read or is of another schema, or the server
/// that wrote it is no longer alive: a server killed outright leaves
/// its status behind.
+ (nullable instancetype)load;
/// Decodes a status. Returns nil for anything but a schema 1 status.
+ (nullable instancetype)statusWithData:(NSData *)data;

@property (nonatomic, readonly) NSInteger pid;
@property (nonatomic, copy, readonly) NSArray<RuneStatusWorkspace *> *workspaces;
@property (nonatomic, copy, readonly) NSString *networkState;
@property (nonatomic, copy, readonly) NSString *hostname;
@property (nonatomic, copy, readonly) NSString *loginName;
@property (nonatomic, copy, readonly) NSString *lastError;
@property (nonatomic, copy, readonly) NSString *authURL;
@property (nonatomic, copy, readonly) NSArray<RuneStatusPeer *> *peers;

/// Whether the server that published this status is still running.
@property (nonatomic, readonly) BOOL serverAlive;
/// How many terminals, across every workspace, still run a program.
@property (nonatomic, readonly) NSUInteger liveTerminalCount;
@property (nonatomic, readonly) RuneNetworkState network;

@end

NS_ASSUME_NONNULL_END
