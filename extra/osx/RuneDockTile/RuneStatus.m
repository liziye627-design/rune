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

#import "RuneStatus.h"

#include <errno.h>
#include <signal.h>

static const NSInteger kRuneStatusSchema = 1;

// JSON helpers tolerant of a field that is missing or null: the server
// writes every field, but a reader that crashes the Dock over one is
// worse than one that shows less.
static NSString *runeString(NSDictionary *dict, NSString *key) {
  id value = dict[key];
  return [value isKindOfClass:[NSString class]] ? value : @"";
}

static NSInteger runeInteger(NSDictionary *dict, NSString *key) {
  id value = dict[key];
  return [value isKindOfClass:[NSNumber class]] ? [value integerValue] : 0;
}

static BOOL runeBool(NSDictionary *dict, NSString *key) {
  id value = dict[key];
  return [value isKindOfClass:[NSNumber class]] ? [value boolValue] : NO;
}

static NSArray *runeArray(NSDictionary *dict, NSString *key) {
  id value = dict[key];
  return [value isKindOfClass:[NSArray class]] ? value : @[];
}

static NSArray<NSString *> *runeStrings(NSDictionary *dict, NSString *key) {
  NSMutableArray<NSString *> *ret = [NSMutableArray array];
  for (id value in runeArray(dict, key)) {
    if ([value isKindOfClass:[NSString class]]) {
      [ret addObject:value];
    }
  }
  return ret;
}

@interface RuneStatusPeer ()
- (instancetype)initWithDictionary:(NSDictionary *)dict;
@end

@implementation RuneStatusPeer
- (instancetype)initWithDictionary:(NSDictionary *)dict {
  self = [super init];
  if (self) {
    _hostname = runeString(dict, @"hostname");
    _dnsName = runeString(dict, @"dns_name");
    _os = runeString(dict, @"os");
    _online = runeBool(dict, @"online");
  }
  return self;
}
@end

@interface RuneStatusTerminal ()
- (instancetype)initWithDictionary:(NSDictionary *)dict;
@end

@implementation RuneStatusTerminal
- (instancetype)initWithDictionary:(NSDictionary *)dict {
  self = [super init];
  if (self) {
    _identifier = runeString(dict, @"id");
    _title = runeString(dict, @"title");
    _commandAndArgs = runeStrings(dict, @"command_and_args");
    _attachments = runeInteger(dict, @"attachments");
    _complete = runeBool(dict, @"complete");
  }
  return self;
}

- (NSString *)displayName {
  if ([_title length] > 0) {
    return _title;
  }
  if ([_commandAndArgs count] > 0) {
    return [_commandAndArgs componentsJoinedByString:@" "];
  }
  return @"shell";
}
@end

@interface RuneStatusWorkspace ()
- (instancetype)initWithDictionary:(NSDictionary *)dict;
@end

@implementation RuneStatusWorkspace
- (instancetype)initWithDictionary:(NSDictionary *)dict {
  self = [super init];
  if (self) {
    _root = runeString(dict, @"root");
    NSMutableArray<RuneStatusTerminal *> *terminals = [NSMutableArray array];
    for (id entry in runeArray(dict, @"vtes")) {
      if ([entry isKindOfClass:[NSDictionary class]]) {
        [terminals addObject:[[RuneStatusTerminal alloc] initWithDictionary:entry]];
      }
    }
    _terminals = terminals;
  }
  return self;
}
@end

@implementation RuneStatus

+ (NSString *)dataDirectory {
  return [NSHomeDirectory() stringByAppendingPathComponent:@".rune"];
}

+ (NSString *)serversDirectory {
  return [[self dataDirectory] stringByAppendingPathComponent:@"servers"];
}

+ (NSString *)statusPath {
  return [[self serversDirectory] stringByAppendingPathComponent:@"status.json"];
}

+ (NSString *)serverLogPath {
  return [[self dataDirectory] stringByAppendingPathComponent:@"server.log"];
}

+ (nullable instancetype)load {
  NSData *data = [NSData dataWithContentsOfFile:[self statusPath]];
  if (data == nil) {
    return nil;
  }
  RuneStatus *status = [self statusWithData:data];
  return status.serverAlive ? status : nil;
}

+ (nullable instancetype)statusWithData:(NSData *)data {
  NSError *error = nil;
  id root = [NSJSONSerialization JSONObjectWithData:data options:0 error:&error];
  if (![root isKindOfClass:[NSDictionary class]]) {
    NSLog(@"RuneDockTile: status is not an object: %@", error);
    return nil;
  }
  if (runeInteger(root, @"schema") != kRuneStatusSchema) {
    NSLog(@"RuneDockTile: status schema %ld, want %ld",
          (long)runeInteger(root, @"schema"), (long)kRuneStatusSchema);
    return nil;
  }
  return [[self alloc] initWithDictionary:root];
}

- (instancetype)initWithDictionary:(NSDictionary *)dict {
  self = [super init];
  if (self) {
    _pid = runeInteger(dict, @"pid");
    NSMutableArray<RuneStatusWorkspace *> *workspaces = [NSMutableArray array];
    for (id entry in runeArray(dict, @"workspaces")) {
      if ([entry isKindOfClass:[NSDictionary class]]) {
        [workspaces addObject:[[RuneStatusWorkspace alloc] initWithDictionary:entry]];
      }
    }
    _workspaces = workspaces;

    id network = dict[@"network"];
    if (![network isKindOfClass:[NSDictionary class]]) {
      network = @{};
    }
    _networkState = runeString(network, @"state");
    _hostname = runeString(network, @"hostname");
    _loginName = runeString(network, @"login_name");
    _lastError = runeString(network, @"last_error");
    _authURL = runeString(network, @"auth_url");
    NSMutableArray<RuneStatusPeer *> *peers = [NSMutableArray array];
    for (id entry in runeArray(network, @"peers")) {
      if ([entry isKindOfClass:[NSDictionary class]]) {
        [peers addObject:[[RuneStatusPeer alloc] initWithDictionary:entry]];
      }
    }
    _peers = peers;
  }
  return self;
}

- (BOOL)serverAlive {
  if (_pid <= 0) {
    return NO;
  }
  // EPERM is a process that exists but is not ours to signal, which a
  // server of this user never is.
  return kill((pid_t)_pid, 0) == 0;
}

- (NSUInteger)liveTerminalCount {
  NSUInteger count = 0;
  for (RuneStatusWorkspace *workspace in _workspaces) {
    for (RuneStatusTerminal *terminal in workspace.terminals) {
      if (!terminal.complete) {
        count++;
      }
    }
  }
  return count;
}

- (RuneNetworkState)network {
  if ([_networkState isEqualToString:@"Running"]) {
    return RuneNetworkStateUp;
  }
  if ([_networkState isEqualToString:@"NeedsLogin"] || [_authURL length] > 0) {
    return RuneNetworkStateSignIn;
  }
  if ([_networkState length] == 0 || [_networkState isEqualToString:@"NotRunning"] ||
      [_networkState isEqualToString:@"Stopped"]) {
    // A node that is off because it could not join needs the user;
    // one that is simply off does not.
    return [_lastError length] > 0 ? RuneNetworkStateError : RuneNetworkStateDown;
  }
  return RuneNetworkStateError;
}

@end
