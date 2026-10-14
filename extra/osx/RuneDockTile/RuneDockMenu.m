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

#import "RuneDockMenu.h"

// A dot is a filled oval a little smaller than the menu's text, with
// room around it so it sits beside the title rather than against it.
static const CGFloat kRuneDotDiameter = 8;
static const CGFloat kRuneDotPadding = 2;

@implementation RuneDockMenu {
  __weak id<RuneDockMenuActions> _target;
}

- (instancetype)initWithTarget:(id<RuneDockMenuActions>)target {
  self = [super init];
  if (self) {
    _target = target;
  }
  return self;
}

+ (nullable NSImage *)imageForDot:(RuneDot)dot {
  NSColor *color;
  switch (dot) {
  case RuneDotNone:
    return nil;
  case RuneDotGreen:
    color = [NSColor systemGreenColor];
    break;
  case RuneDotGray:
    color = [NSColor tertiaryLabelColor];
    break;
  case RuneDotAmber:
    color = [NSColor systemOrangeColor];
    break;
  }
  CGFloat side = kRuneDotDiameter + 2 * kRuneDotPadding;
  NSImage *image = [NSImage imageWithSize:NSMakeSize(side, side)
                                  flipped:NO
                           drawingHandler:^BOOL(NSRect rect) {
                             NSRect oval = NSInsetRect(rect, kRuneDotPadding, kRuneDotPadding);
                             [color setFill];
                             [[NSBezierPath bezierPathWithOvalInRect:oval] fill];
                             return YES;
                           }];
  // The handler reads the appearance each time it draws, so the dot
  // follows dark mode and the menu's own appearance.
  return image;
}

// The Dock has no responder chain to send an action down, so every
// item names its target.
- (NSMenuItem *)item:(NSString *)title
              action:(nullable SEL)action
         represented:(nullable id)object {
  NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:title
                                                action:action
                                         keyEquivalent:@""];
  if (action != NULL) {
    item.target = _target;
  }
  item.representedObject = object;
  return item;
}

- (NSMenuItem *)label:(NSString *)title dot:(RuneDot)dot {
  NSMenuItem *item = [self item:title action:NULL represented:nil];
  item.enabled = NO;
  item.image = [RuneDockMenu imageForDot:dot];
  return item;
}

- (NSMenuItem *)label:(NSString *)title {
  return [self label:title dot:RuneDotNone];
}

- (NSMenu *)menuForStatus:(nullable RuneStatus *)status {
  NSMenu *menu = [[NSMenu alloc] initWithTitle:@"Rune"];
  // Items decide for themselves: a disabled item is one with no
  // action, and the ones with one are always available.
  menu.autoenablesItems = NO;
  if (status == nil) {
    [menu addItem:[self label:@"No background terminals" dot:RuneDotGray]];
    return menu;
  }

  [self addNetworkTo:menu status:status];
  [menu addItem:[NSMenuItem separatorItem]];
  [self addTerminalsTo:menu status:status];
  [menu addItem:[self item:@"Open Server Log" action:@selector(openServerLog:) represented:nil]];
  [menu addItem:[NSMenuItem separatorItem]];
  [menu addItem:[self item:@"Quit Background Terminals\u2026"
                    action:@selector(stopServer:)
               represented:@(status.liveTerminalCount)]];
  return menu;
}

- (void)addNetworkTo:(NSMenu *)menu status:(RuneStatus *)status {
  switch (status.network) {
  case RuneNetworkStateUp:
    [menu addItem:[self label:[NSString stringWithFormat:@"Connected to Rune network as %@",
                                                         status.hostname]
                         dot:RuneDotGreen]];
    break;
  case RuneNetworkStateDown:
    [menu addItem:[self label:@"Not connected to Rune network" dot:RuneDotGray]];
    break;
  case RuneNetworkStateSignIn: {
    // The one row that does something: the sign-in page is the only
    // way out of this state, and the Dock can open it.
    NSMenuItem *item = [self item:@"Rune network: sign-in required"
                           action:[status.authURL length] > 0 ? @selector(signIn:) : NULL
                      represented:status.authURL];
    item.enabled = [status.authURL length] > 0;
    item.image = [RuneDockMenu imageForDot:RuneDotAmber];
    [menu addItem:item];
    break;
  }
  case RuneNetworkStateError:
    [menu addItem:[self label:[NSString stringWithFormat:@"Rune network: %@",
                                                         [status.lastError length] > 0
                                                             ? status.lastError
                                                             : status.networkState]
                         dot:RuneDotAmber]];
    break;
  }

  NSMenuItem *peers = [self item:@"Peers" action:NULL represented:nil];
  peers.submenu = [self peersMenu:status];
  [menu addItem:peers];

  if (status.network == RuneNetworkStateUp) {
    [menu addItem:[self item:@"Disconnect from Network"
                      action:@selector(disconnectNetwork:)
                 represented:nil]];
  } else {
    [menu addItem:[self item:@"Connect to Network"
                      action:@selector(connectNetwork:)
                 represented:nil]];
  }
}

- (NSMenu *)peersMenu:(RuneStatus *)status {
  NSMenu *menu = [[NSMenu alloc] initWithTitle:@"Peers"];
  menu.autoenablesItems = NO;
  if ([status.peers count] == 0) {
    [menu addItem:[self label:@"No other machines"]];
    return menu;
  }
  for (RuneStatusPeer *peer in status.peers) {
    NSString *title = [peer.os length] > 0
                          ? [NSString stringWithFormat:@"%@  %@", peer.hostname, peer.os]
                          : peer.hostname;
    NSMenuItem *item = [self item:title
                           action:@selector(openPeer:)
                      represented:peer.hostname];
    item.image = [RuneDockMenu imageForDot:peer.online ? RuneDotGreen : RuneDotGray];
    item.toolTip = [NSString stringWithFormat:@"rune://%@/", peer.hostname];
    [menu addItem:item];
  }
  return menu;
}

- (void)addTerminalsTo:(NSMenu *)menu status:(RuneStatus *)status {
  NSMenuItem *terminals = [self item:@"Terminals" action:NULL represented:nil];
  NSMenu *submenu = [[NSMenu alloc] initWithTitle:@"Terminals"];
  submenu.autoenablesItems = NO;
  for (RuneStatusWorkspace *workspace in status.workspaces) {
    if ([workspace.terminals count] == 0) {
      continue;
    }
    if ([submenu numberOfItems] > 0) {
      [submenu addItem:[NSMenuItem separatorItem]];
    }
    [submenu addItem:[self label:[workspace.root stringByAbbreviatingWithTildeInPath]]];
    for (RuneStatusTerminal *terminal in workspace.terminals) {
      NSString *name = terminal.displayName;
      if (terminal.complete) {
        name = [name stringByAppendingString:@" (exited)"];
      }
      [submenu addItem:[self item:name
                           action:@selector(openWorkspace:)
                      represented:workspace.root]];
    }
  }
  if ([submenu numberOfItems] == 0) {
    [submenu addItem:[self label:@"No terminals"]];
  }
  terminals.submenu = submenu;
  [menu addItem:terminals];
}

@end
