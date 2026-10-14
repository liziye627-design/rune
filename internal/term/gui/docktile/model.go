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

// Package docktile drives the application's Dock tile on macOS: the
// badge on the icon and the menu the Dock shows for a running
// application. Elsewhere it does nothing.
//
// It is the running application's half of the Dock tile; while the
// application is not running, the Dock shows the plug-in the app
// bundle ships (extra/osx/RuneDockTile) instead.
package docktile

import "sync"

// Dot is the coloured dot before a menu item's title, showing the state
// of what the item names.
type Dot int

// The dots: none, and the three states a network or a peer can be in.
const (
	DotNone Dot = iota
	DotGreen
	DotGray
	DotAmber
)

// Item is one entry of the Dock menu. An item with Children is a
// submenu; a Separator is a rule; any other item is a row, which is
// clickable when it carries a Tag and is not Disabled.
type Item struct {
	Title    string
	Disabled bool
	Dot      Dot
	Children []Item
	// Tag identifies the item to the activate callback of SetMenu. It
	// must be positive: zero is AppKit's default and names nothing.
	Tag       int
	Separator bool
}

// Submenu is an item that expands into items.
func Submenu(title string, items ...Item) Item {
	if items == nil {
		items = []Item{}
	}
	return Item{Title: title, Children: items}
}

// Label is a row that only says something.
func Label(title string, dot Dot) Item {
	return Item{Title: title, Disabled: true, Dot: dot}
}

// Separator is a rule between items.
func Separator() Item {
	return Item{Separator: true}
}

// itemSpec is the flattened description handed to the platform layer,
// which keeps the Objective-C layer free of any logic.
type itemSpec struct {
	title     string
	tag       int
	disabled  bool
	dot       int
	separator bool
	children  []itemSpec
}

var (
	mu        sync.Mutex
	activated func(int)
)

// SetBadge shows label on the Dock icon; an empty label removes the
// badge. It must be called on the main thread.
func SetBadge(label string) {
	setBadge(label)
}

// SetMenu replaces the Dock menu with items. Clicking an item invokes
// activate with its Tag, on the main thread. The platform appends its
// own standard items — Hide, Quit — after these.
//
// It must be called on the main thread.
func SetMenu(items []Item, activate func(tag int)) {
	mu.Lock()
	activated = activate
	mu.Unlock()

	installMenu(buildSpec(items))
}

// activateTag runs the callback registered by the most recent SetMenu.
func activateTag(tag int) {
	mu.Lock()
	activate := activated
	mu.Unlock()
	if activate != nil {
		activate(tag)
	}
}

func buildSpec(items []Item) []itemSpec {
	specs := make([]itemSpec, 0, len(items))
	for _, item := range items {
		switch {
		case item.Separator:
			specs = append(specs, itemSpec{separator: true})
		case item.Children != nil:
			specs = append(specs, itemSpec{
				title:    item.Title,
				dot:      int(item.Dot),
				children: buildSpec(item.Children),
			})
		default:
			specs = append(specs, itemSpec{
				title: item.Title,
				tag:   item.Tag,
				// A row with nothing to do is greyed out whatever it
				// says, so the menu never offers a click that does
				// nothing.
				disabled: item.Disabled || item.Tag == 0,
				dot:      int(item.Dot),
			})
		}
	}
	return specs
}
