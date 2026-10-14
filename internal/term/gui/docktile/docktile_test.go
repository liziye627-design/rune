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

package docktile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildSpec(t *testing.T) {
	t.Parallel()
	got := buildSpec([]Item{
		Label("Connected to Rune network as laptop", DotGreen),
		Submenu("Peers",
			Item{Title: "desk  linux", Dot: DotGreen, Tag: 1},
			Item{Title: "pi  linux", Dot: DotGray, Tag: 2},
		),
		Item{Title: "Disconnect from Network", Tag: 3},
		Separator(),
		Submenu("Terminals"),
		Item{Title: "Nothing to do"},
		Item{Title: "Greyed out", Tag: 4, Disabled: true},
	})
	assert.Equal(t, []itemSpec{
		{title: "Connected to Rune network as laptop", disabled: true, dot: int(DotGreen)},
		{title: "Peers", children: []itemSpec{
			{title: "desk  linux", tag: 1, dot: int(DotGreen)},
			{title: "pi  linux", tag: 2, dot: int(DotGray)},
		}},
		{title: "Disconnect from Network", tag: 3},
		{separator: true},
		{title: "Terminals", children: []itemSpec{}},
		{title: "Nothing to do", disabled: true},
		{title: "Greyed out", tag: 4, disabled: true},
	}, got)
}

// A click reaches the callback of the menu set last, and a menu set
// with no callback swallows clicks rather than reaching a stale one.
func TestActivateTag(t *testing.T) {
	var got []int
	SetMenu([]Item{{Title: "one", Tag: 1}}, func(tag int) { got = append(got, tag) })
	activateTag(1)
	SetMenu([]Item{{Title: "two", Tag: 2}}, func(tag int) { got = append(got, tag*10) })
	activateTag(2)
	SetMenu(nil, nil)
	activateTag(2)
	assert.Equal(t, []int{1, 20}, got)
}
