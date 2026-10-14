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
#import <objc/runtime.h>

#include "_cgo_export.h"

@interface RuneDockMenuTarget : NSObject
- (void)runeDockMenuAction:(id)sender;
@end

@implementation RuneDockMenuTarget
- (void)runeDockMenuAction:(id)sender {
  runeDockMenuActivate((int)[(NSMenuItem *)sender tag]);
}
@end

// All state below is touched only from the main thread, which is the
// documented contract of runeDock* and where AppKit asks the delegate
// for the Dock menu anyway.
static RuneDockMenuTarget *gTarget = nil;
// gMenu is what the Dock is handed; gBuilding is the one being filled,
// so the Dock never sees a half-built menu.
static NSMenu *gMenu = nil;
static NSMenu *gBuilding = nil;
static NSMutableArray<NSMenu *> *gMenuStack = nil;

// Dots match the plug-in's: an 8pt oval with 2pt around it.
static const CGFloat kRuneDotDiameter = 8;
static const CGFloat kRuneDotPadding = 2;

enum { kRuneDotNone = 0, kRuneDotGreen, kRuneDotGray, kRuneDotAmber };

static NSImage *runeDotImage(int dot) {
  NSColor *color;
  switch (dot) {
  case kRuneDotGreen:
    color = [NSColor systemGreenColor];
    break;
  case kRuneDotGray:
    color = [NSColor tertiaryLabelColor];
    break;
  case kRuneDotAmber:
    color = [NSColor systemOrangeColor];
    break;
  default:
    return nil;
  }
  CGFloat side = kRuneDotDiameter + 2 * kRuneDotPadding;
  return [NSImage imageWithSize:NSMakeSize(side, side)
                        flipped:NO
                 drawingHandler:^BOOL(NSRect rect) {
                   NSRect oval = NSInsetRect(rect, kRuneDotPadding, kRuneDotPadding);
                   [color setFill];
                   [[NSBezierPath bezierPathWithOvalInRect:oval] fill];
                   return YES;
                 }];
}

static NSMenu *runeCurrentMenu(void) {
  return [gMenuStack lastObject];
}

// runeApplicationDockMenu is the applicationDockMenu: the application
// delegate gains. AppKit calls it every time the Dock menu is about to
// show, so the menu set last is the one shown.
static NSMenu *runeApplicationDockMenu(id self, SEL _cmd, NSApplication *sender) {
  return gMenu;
}

// runeInstallDockMenuDelegate teaches the application delegate — the
// windowing library's, which has no Dock menu of its own — to answer
// for the Dock menu. Without a delegate there is nobody to ask, and
// the Dock shows its standard items only.
static void runeInstallDockMenuDelegate(void) {
  id delegate = [NSApp delegate];
  if (delegate == nil) {
    return;
  }
  Class cls = object_getClass(delegate);
  SEL sel = @selector(applicationDockMenu:);
  if (class_getInstanceMethod(cls, sel) != NULL) {
    return;
  }
  class_addMethod(cls, sel, (IMP)runeApplicationDockMenu, "@@:@");
  // NSApplication notes what its delegate answers to when the
  // delegate is set, so it is set again now that it answers to more.
  [NSApp setDelegate:nil];
  [NSApp setDelegate:delegate];
}

void runeDockTileSetBadge(const char *label) {
  // Calls from Go run outside any autorelease pool, so each brings its
  // own or what it autoreleases is never freed.
  @autoreleasepool {
    NSDockTile *tile = [NSApp dockTile];
    [tile setBadgeLabel:(label != NULL ? [NSString stringWithUTF8String:label] : nil)];
    [tile display];
  }
}

void runeDockMenuBegin(void) {
  if (gTarget == nil) {
    gTarget = [[RuneDockMenuTarget alloc] init];
  }
  [gBuilding release];
  gBuilding = [[NSMenu alloc] initWithTitle:@""];
  // Items decide for themselves: a disabled item is one with no
  // action, and the ones with one are always available.
  [gBuilding setAutoenablesItems:NO];
  if (gMenuStack == nil) {
    gMenuStack = [[NSMutableArray alloc] init];
  }
  [gMenuStack removeAllObjects];
  [gMenuStack addObject:gBuilding];
}

void runeDockMenuBeginSubmenu(const char *title, int dot) {
  @autoreleasepool {
    NSString *name = [NSString stringWithUTF8String:title];
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:name action:NULL keyEquivalent:@""];
    [item setImage:runeDotImage(dot)];
    NSMenu *menu = [[NSMenu alloc] initWithTitle:name];
    [menu setAutoenablesItems:NO];
    [item setSubmenu:menu];
    [runeCurrentMenu() addItem:item];
    [gMenuStack addObject:menu];
    [menu release];
    [item release];
  }
}

void runeDockMenuEndSubmenu(void) {
  if ([gMenuStack count] > 1) {
    [gMenuStack removeLastObject];
  }
}

void runeDockMenuAddSeparator(void) {
  @autoreleasepool {
    [runeCurrentMenu() addItem:[NSMenuItem separatorItem]];
  }
}

void runeDockMenuAddItem(const char *title, int tag, int disabled, int dot) {
  @autoreleasepool {
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:[NSString stringWithUTF8String:title]
                                                  action:NULL
                                           keyEquivalent:@""];
    [item setImage:runeDotImage(dot)];
    if (disabled) {
      [item setEnabled:NO];
    } else {
      [item setTag:tag];
      [item setTarget:gTarget];
      [item setAction:@selector(runeDockMenuAction:)];
    }
    [runeCurrentMenu() addItem:item];
    [item release];
  }
}

void runeDockMenuCommit(void) {
  // The menu is rebuilt on every status change. AppKit retains the
  // one it is showing, so the previous one is this file's to release.
  [gMenu release];
  gMenu = gBuilding;
  gBuilding = nil;
  [gMenuStack removeAllObjects];
  runeInstallDockMenuDelegate();
}
