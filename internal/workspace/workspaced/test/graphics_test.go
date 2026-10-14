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

//go:build e2e

package workspacedtest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// graphicsCols and graphicsRows are the size of the terminal the
// graphics tests draw in, wide enough for the program's report.
const (
	graphicsCols = 80
	graphicsRows = 12
)

// kittyProgram draws with the kitty graphics protocol the way an image
// viewer does. For each "image <winsize> <label>" line it reads, it
// clears the screen, hands the terminal a 20x40 pixel image as a
// temporary file to read and delete (t=t), prints an x after it, and
// reports below it:
//
//   - answers: what the terminal answered, ESC spelt E, to where its
//     cursor is, the size of a cell and of the text area in pixels, and
//     whether it supports graphics;
//   - tmpfile: whether the terminal deleted the file;
//   - winsize: the pty's size, in cells and pixels, once it is <winsize>
//     or 5s passed.
//
// It ends each with "done=<label>".
const kittyProgram = `winsize() {
	perl -e '
		$want = shift;
		$req = $^O eq "darwin" ? 0x40087468 : 0x5413;
		for (1 .. 100) {
			$ws = "\0" x 8;
			ioctl(STDIN, $req, $ws) or die "TIOCGWINSZ: $!\n";
			($rows, $cols, $x, $y) = unpack("S4", $ws);
			$got = "${cols}x${rows}/${x}x${y}";
			last if $got eq $want;
			select(undef, undef, undef, 0.05);
		}
		print "winsize=$got\n";
	' "$1"
}

echo ready
while read cmd want label; do
	[ "$cmd" = image ] || continue
	old=$(stty -g)
	stty raw -echo min 0 time 50
	f=$(mktemp /tmp/tty-graphics-protocol-XXXXXX)
	head -c 2400 /dev/zero > "$f"
	printf '\033[H\033[2J\033_Ga=T,f=24,s=20,v=40,t=t;%s\033\\x' \
		"$(printf %s "$f" | base64 | tr -d '\n')"
	printf '\033[6n\033[16t\033[14t\033_Gi=31,a=q,s=1,v=1,f=24;AAAA\033\\\033[5n'
	answers=
	while c=$(dd bs=1 count=1 2>/dev/null) && [ -n "$c" ] && [ "$c" != n ]; do
		answers=$answers$c
	done
	stty "$old"
	printf '\033[6;1H'
	printf 'answers=%s\n' "$answers" | tr '\033' E
	if [ -e "$f" ]; then echo tmpfile=kept; rm -f "$f"; else echo tmpfile=deleted; fi
	winsize "$want"
	echo "done=$label"
done
`

// graphicsDisplay carries the browser of a display whose cells are
// width by height pixels, or of a cells-only one, as rune --tui is, when
// they are zero.
func graphicsDisplay(width, height int) context.Context {
	cfg := vte.DefaultConfig()
	cfg.WidthHint, cfg.HeightHint = graphicsCols, graphicsRows
	if width > 0 && height > 0 {
		cfg.CellPixelSize = func() (int, int) { return width, height }
	}
	return vtescheme.WithClientVTEEnv(context.Background(), vtescheme.ClientVTEEnv{
		Publisher:     workspacetest.NopVTEHost{},
		Notifications: workspacetest.NopVTEHost{},
		Tabs:          workspacetest.NopVTEHost{},
		Config:        cfg,
	})
}

// TestTerminalsDrawKittyGraphics pins kitty graphics in the terminals of
// a machine's server, which a program talks to through the server's
// emulator while every attached Rune replays it on a display of its own.
func TestTerminalsDrawKittyGraphics(t *testing.T) {
	t.Parallel()
	m := newMachine(t, "")
	ws := m.workspace(t, "ws")
	program := script(t, ws, "kitty.sh", kittyProgram)
	assertKittyGraphics(t, program, [3]vtescheme.Scheme{
		m.client(t, ws), m.client(t, ws), m.client(t, ws),
	})
}

// TestSSHTerminalsDrawKittyGraphics pins kitty graphics over ssh, where
// the file a program hands the terminal only exists on the remote
// machine: the Runes replaying the terminal could not read it even if
// the terminal had not deleted it.
func TestSSHTerminalsDrawKittyGraphics(t *testing.T) {
	c := startContainer(t)
	t.Parallel()
	dir := home + "/ws"
	clients := [3]vtescheme.Scheme{c.client(t, dir), c.client(t, dir), c.client(t, dir)}
	program := dir + "/kitty.sh"
	c.sh(t, "cat > "+program+" <<'EOF'\n"+kittyProgram+"EOF\n")
	assertKittyGraphics(t, program, clients)
}

// assertKittyGraphics runs kittyProgram, at program on the machine of
// clients, in a terminal the first client opens from a display of 10x20
// pixel cells while the others watch it, from a display of 8x16 pixel
// cells and a cells-only one.
//
// The size of a cell in pixels is terminal state, decided like the grid
// by whichever client holds resize authority. The server's emulator
// answers with it and gives it to the pty, and every client places the
// image with it, so the x after the image lands where the server's
// cursor is on every display.
func assertKittyGraphics(t *testing.T, program string, clients [3]vtescheme.Scheme) {
	t.Helper()
	owner, err := clients[0].CreateVTE(graphicsDisplay(10, 20), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"/bin/sh", program},
		Width:          graphicsCols,
		Height:         graphicsRows,
		MaxLines:       1000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })
	awaitScreen(t, owner, "ready")
	small, err := clients[1].AttachVTE(graphicsDisplay(8, 16), owner.ID())
	require.NoError(t, err)
	t.Cleanup(func() { _ = small.Close() })
	cellsOnly, err := clients[2].AttachVTE(graphicsDisplay(0, 0), owner.ID())
	require.NoError(t, err)
	t.Cleanup(func() { _ = cellsOnly.Close() })

	for _, step := range []struct {
		name string
		// before hands resize authority to whoever it goes to.
		before func(t *testing.T)
		// typist is the client the program's input is typed on.
		typist   workspace.VTE
		watchers []workspace.VTE
		// cellWidth and cellHeight are the cell size in force.
		cellWidth, cellHeight int
	}{
		{
			name:      "the first client's cell",
			before:    func(*testing.T) {},
			typist:    owner,
			watchers:  []workspace.VTE{owner, small, cellsOnly},
			cellWidth: 10, cellHeight: 20,
		},
		{
			name:      "the cell of the client that takes focus",
			before:    func(*testing.T) { small.OnFocusChange(true) },
			typist:    small,
			watchers:  []workspace.VTE{owner, small, cellsOnly},
			cellWidth: 8, cellHeight: 16,
		},
		{
			name: "the heir's cell once its holder leaves",
			before: func(t *testing.T) {
				require.NoError(t, small.Close())
				eventually(t, "the server to drop the client", func() (bool, string) {
					infos, err := clients[0].ListVTEs(context.Background())
					ok := err == nil && len(infos) == 1 && infos[0].Attachments == 2
					return ok, fmt.Sprint(infos, err)
				})
			},
			typist:    owner,
			watchers:  []workspace.VTE{owner, cellsOnly},
			cellWidth: 10, cellHeight: 20,
		},
		{
			// A cells-only display has no cell to ask for.
			name:      "the cell in force when a cells-only client takes focus",
			before:    func(*testing.T) { cellsOnly.OnFocusChange(true) },
			typist:    cellsOnly,
			watchers:  []workspace.VTE{owner, cellsOnly},
			cellWidth: 10, cellHeight: 20,
		},
	} {
		t.Run(step.name, func(t *testing.T) {
			step.before(t)
			w, h := step.cellWidth, step.cellHeight
			label := strings.ReplaceAll(step.name, " ", "-")
			typeLine(step.typist, fmt.Sprintf("image %dx%d/%dx%d %s",
				graphicsCols, graphicsRows, graphicsCols*w, graphicsRows*h, label))

			// The image covers ceil(20/w) by ceil(40/h) cells and leaves
			// the cursor after its last cell on its last row.
			cols, rows := (20+w-1)/w, (40+h-1)/h
			report := []string{
				fmt.Sprintf("answers=E[%d;%dRE[6;%d;%dtE[4;%d;%dtE_Gi=31;OKE\\E[0",
					rows, cols+2, h, w, graphicsRows*h, graphicsCols*w),
				"tmpfile=deleted",
				fmt.Sprintf("winsize=%dx%d/%dx%d",
					graphicsCols, graphicsRows, graphicsCols*w, graphicsRows*h),
			}
			for i, v := range step.watchers {
				awaitScreen(t, v, "done="+label)
				got := screen(v)
				lines := visibleRows(t, v)
				for y := range rows - 1 {
					assert.Empty(t, strings.TrimSpace(lines[y]),
						"client %d: row %d is under the image\n%s", i, y, got)
				}
				assert.Equal(t, strings.Repeat(" ", cols)+"x",
					strings.TrimRight(lines[rows-1], " "),
					"client %d: the x follows the image\n%s", i, got)
				for _, line := range report {
					assert.Contains(t, got, line, "client %d", i)
				}
			}
		})
	}
}

// visibleRows is the text of the rows a terminal shows, without the
// history above them.
func visibleRows(t *testing.T, v workspace.VTE) []string {
	t.Helper()
	snap, err := v.Snapshot()
	require.NoError(t, err)
	cells := snap.ActiveCells()
	require.GreaterOrEqual(t, len(cells), snap.Height)
	ret := make([]string, 0, snap.Height)
	for _, row := range cells[len(cells)-snap.Height:] {
		var b strings.Builder
		for _, cell := range row {
			if cell.Ch == 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(cell.Ch)
		}
		ret = append(ret, b.String())
	}
	return ret
}
