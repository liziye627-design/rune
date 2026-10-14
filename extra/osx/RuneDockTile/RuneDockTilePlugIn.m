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

#import "RuneDockTilePlugIn.h"

#import "RuneControl.h"
#import "RuneStatus.h"

#include <fcntl.h>
#include <unistd.h>

// The status file is replaced by rename, so the directory holding it
// is what is watched: a watch on the file itself would follow the old
// inode into oblivion.
static const uint64_t kRuneWatchTimerInterval = 30 * NSEC_PER_SEC;
// A write is a temp file plus a rename; one refresh covers both.
static const int64_t kRuneRefreshDelay = 100 * NSEC_PER_MSEC;
// A command is answered at once unless it joins the network, which can
// wait on a login nobody completes; the Dock must not.
static const NSTimeInterval kRuneControlTimeout = 2.0;

@implementation RuneDockTilePlugIn {
  NSDockTile *_tile;
  RuneDockMenu *_menu;
  dispatch_source_t _watch;
  dispatch_source_t _timer;
  BOOL _refreshPending;
}

- (instancetype)init {
  self = [super init];
  if (self) {
    _menu = [[RuneDockMenu alloc] initWithTarget:self];
  }
  return self;
}

- (void)dealloc {
  [self stopWatching];
}

#pragma mark - NSDockTilePlugIn

- (void)setDockTile:(nullable NSDockTile *)dockTile {
  _tile = dockTile;
  if (dockTile == nil) {
    [self stopWatching];
    return;
  }
  [self startWatching];
  [self refresh];
}

- (nullable NSMenu *)dockMenu {
  return [_menu menuForStatus:[RuneStatus load]];
}

#pragma mark - Watching the status

- (void)startWatching {
  if (_timer != nil) {
    return;
  }
  // The timer is the fallback for a directory that does not exist yet
  // and for a watch that was lost with it.
  _timer = dispatch_source_create(DISPATCH_SOURCE_TYPE_TIMER, 0, 0,
                                  dispatch_get_main_queue());
  __weak RuneDockTilePlugIn *weakSelf = self;
  dispatch_source_set_timer(_timer, dispatch_time(DISPATCH_TIME_NOW, kRuneWatchTimerInterval),
                            kRuneWatchTimerInterval, NSEC_PER_SEC);
  dispatch_source_set_event_handler(_timer, ^{
    [weakSelf watchDirectory];
    [weakSelf refresh];
  });
  dispatch_resume(_timer);
  [self watchDirectory];
}

- (void)stopWatching {
  if (_timer != nil) {
    dispatch_source_cancel(_timer);
    _timer = nil;
  }
  [self stopWatchingDirectory];
}

- (void)watchDirectory {
  if (_watch != nil) {
    return;
  }
  int fd = open([[RuneStatus serversDirectory] fileSystemRepresentation], O_EVTONLY);
  if (fd < 0) {
    return;
  }
  dispatch_source_t watch = dispatch_source_create(
      DISPATCH_SOURCE_TYPE_VNODE, (uintptr_t)fd,
      DISPATCH_VNODE_WRITE | DISPATCH_VNODE_DELETE | DISPATCH_VNODE_RENAME,
      dispatch_get_main_queue());
  if (watch == nil) {
    close(fd);
    return;
  }
  __weak RuneDockTilePlugIn *weakSelf = self;
  dispatch_source_set_event_handler(watch, ^{
    RuneDockTilePlugIn *plugIn = weakSelf;
    if (plugIn == nil) {
      return;
    }
    unsigned long flags = dispatch_source_get_data(watch);
    if (flags & (DISPATCH_VNODE_DELETE | DISPATCH_VNODE_RENAME)) {
      // The directory itself went away; the timer re-establishes
      // the watch once it is back.
      [plugIn stopWatchingDirectory];
    }
    [plugIn scheduleRefresh];
  });
  dispatch_source_set_cancel_handler(watch, ^{
    close(fd);
  });
  _watch = watch;
  dispatch_resume(watch);
}

- (void)stopWatchingDirectory {
  if (_watch == nil) {
    return;
  }
  // The cancel handler owns the descriptor.
  dispatch_source_cancel(_watch);
  _watch = nil;
}

- (void)scheduleRefresh {
  if (_refreshPending) {
    return;
  }
  _refreshPending = YES;
  __weak RuneDockTilePlugIn *weakSelf = self;
  dispatch_after(dispatch_time(DISPATCH_TIME_NOW, kRuneRefreshDelay),
                 dispatch_get_main_queue(), ^{
                   RuneDockTilePlugIn *plugIn = weakSelf;
                   if (plugIn == nil) {
                     return;
                   }
                   plugIn->_refreshPending = NO;
                   [plugIn refresh];
                 });
}

// refresh reflects the published status on the tile: the number of
// terminals the server keeps running, or nothing when there is no
// server or nothing running.
- (void)refresh {
  if (_tile == nil) {
    return;
  }
  RuneStatus *status = [RuneStatus load];
  NSUInteger count = status != nil ? status.liveTerminalCount : 0;
  NSString *badge = count > 0 ? [NSString stringWithFormat:@"%lu", (unsigned long)count] : nil;
  if ((badge == nil) != (_tile.badgeLabel == nil) || ![badge isEqualToString:_tile.badgeLabel]) {
    _tile.badgeLabel = badge;
    [_tile display];
  }
}

#pragma mark - RuneDockMenuActions

// launchRune starts the Rune this plug-in ships in with args. The
// plug-in only serves the Dock while Rune is not running, so there is
// no running Rune to hand the request to.
- (void)launchRune:(NSArray<NSString *> *)args {
  // Contents/PlugIns/RuneDockTile.docktileplugin → Rune.app
  NSURL *app = [[NSBundle bundleForClass:[self class]] bundleURL];
  for (int i = 0; i < 3; i++) {
    app = [app URLByDeletingLastPathComponent];
  }
  NSWorkspaceOpenConfiguration *config = [NSWorkspaceOpenConfiguration configuration];
  config.arguments = args;
  config.activates = YES;
  [[NSWorkspace sharedWorkspace]
      openApplicationAtURL:app
             configuration:config
         completionHandler:^(NSRunningApplication *_Nullable running, NSError *_Nullable error) {
           if (error != nil) {
             NSLog(@"RuneDockTile: launch %@ %@: %@", app.path, args, error);
           }
         }];
}

- (void)openPeer:(id)sender {
  NSString *hostname = [(NSMenuItem *)sender representedObject];
  if ([hostname length] == 0) {
    return;
  }
  [self launchRune:@[ @"--workspace", [NSString stringWithFormat:@"rune://%@/", hostname] ]];
}

- (void)openWorkspace:(id)sender {
  NSString *root = [(NSMenuItem *)sender representedObject];
  if ([root length] == 0) {
    return;
  }
  [self launchRune:@[ @"--workspace", root ]];
}

- (void)signIn:(id)sender {
  NSString *authURL = [(NSMenuItem *)sender representedObject];
  NSURL *url = [authURL length] > 0 ? [NSURL URLWithString:authURL] : nil;
  if (url != nil) {
    [[NSWorkspace sharedWorkspace] openURL:url];
  }
}

// control sends command to the server off the main thread, which the
// Dock must never be kept from. The outcome shows up in the status the
// server publishes; a failure has nowhere to go but the log.
- (void)control:(NSString *)command {
  dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
    NSError *error = nil;
    if (![RuneControl send:command timeout:kRuneControlTimeout error:&error]) {
      NSLog(@"RuneDockTile: %@: %@", command, error.localizedDescription);
    }
  });
}

- (void)connectNetwork:(id)sender {
  [self control:RuneControlCommandNetworkUp];
}

- (void)disconnectNetwork:(id)sender {
  [self control:RuneControlCommandNetworkDown];
}

- (void)stopServer:(id)sender {
  NSUInteger live = [[(NSMenuItem *)sender representedObject] unsignedIntegerValue];
  NSAlert *alert = [[NSAlert alloc] init];
  alert.alertStyle = NSAlertStyleWarning;
  alert.messageText = @"Quit background terminals?";
  switch (live) {
  case 0:
    alert.informativeText = @"Rune's background server will stop. "
                            @"No terminal is running a program.";
    break;
  case 1:
    alert.informativeText = @"Rune's background server will stop, "
                            @"ending the one terminal still running a program.";
    break;
  default:
    alert.informativeText = [NSString
        stringWithFormat:@"Rune's background server will stop, ending the %lu "
                         @"terminals still running programs.",
                         (unsigned long)live];
    break;
  }
  [alert addButtonWithTitle:@"Quit"];
  [alert addButtonWithTitle:@"Cancel"];
  // The Dock is the frontmost app while its menu is up, but not once
  // the menu closes; the alert must still come to the user.
  [NSApp activateIgnoringOtherApps:YES];
  if ([alert runModal] != NSAlertFirstButtonReturn) {
    return;
  }
  [self control:RuneControlCommandStop];
}

- (void)openServerLog:(id)sender {
  NSURL *log = [NSURL fileURLWithPath:[RuneStatus serverLogPath]];
  [[NSWorkspace sharedWorkspace] openURL:log];
}

@end
