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

//go:build darwin && cgo

package docktile

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa

#include <stdlib.h>

void runeDockTileSetBadge(const char *label);
void runeDockMenuBegin(void);
void runeDockMenuBeginSubmenu(const char *title, int dot);
void runeDockMenuEndSubmenu(void);
void runeDockMenuAddSeparator(void);
void runeDockMenuAddItem(const char *title, int tag, int disabled, int dot);
void runeDockMenuCommit(void);
*/
import "C"

import "unsafe"

func setBadge(label string) {
	if label == "" {
		C.runeDockTileSetBadge(nil)
		return
	}
	cLabel := C.CString(label)
	defer C.free(unsafe.Pointer(cLabel))
	C.runeDockTileSetBadge(cLabel)
}

func installMenu(items []itemSpec) {
	C.runeDockMenuBegin()
	addItems(items)
	C.runeDockMenuCommit()
}

func addItems(items []itemSpec) {
	for _, item := range items {
		switch {
		case item.separator:
			C.runeDockMenuAddSeparator()
		case item.children != nil:
			title := C.CString(item.title)
			C.runeDockMenuBeginSubmenu(title, C.int(item.dot))
			C.free(unsafe.Pointer(title))
			addItems(item.children)
			C.runeDockMenuEndSubmenu()
		default:
			title := C.CString(item.title)
			C.runeDockMenuAddItem(title, C.int(item.tag), cbool(item.disabled), C.int(item.dot))
			C.free(unsafe.Pointer(title))
		}
	}
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

//export runeDockMenuActivate
func runeDockMenuActivate(tag C.int) {
	activateTag(int(tag))
}

// Supported reports whether this build drives a Dock tile.
const Supported = true
