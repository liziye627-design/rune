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

package vtescheme

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/workspace"
)

func TestVTERegistryLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("attach by id returns the same canonical vte", func(t *testing.T) {
		r := newTestVTERegistry(t)
		created, err := r.CreateVTE(ctx, workspace.CreateVTEOptions{
			Width: 20, Height: 10,
		})
		require.NoError(t, err)
		require.NotEmpty(t, created.ID())

		attached, err := r.AttachVTE(ctx, created.ID())
		require.NoError(t, err)
		assert.Equal(t, created.ID(), attached.ID())
		assert.Equal(t, created.URI(), attached.URI())
	})

	t.Run("unknown id is reported as not found", func(t *testing.T) {
		r := newTestVTERegistry(t)
		_, err := r.AttachVTE(ctx, workspace.NewVTEID())
		assert.ErrorIs(t, err, workspace.ErrVTENotFound)
		assert.ErrorIs(t, r.DeleteVTE(ctx, workspace.NewVTEID()),
			workspace.ErrVTENotFound)
	})

	t.Run("detach leaves the canonical vte listed and live", func(t *testing.T) {
		r := newTestVTERegistry(t)
		v, err := r.CreateVTE(ctx, workspace.CreateVTEOptions{
			Width: 20, Height: 10,
		})
		require.NoError(t, err)
		id := v.ID()

		require.NoError(t, v.Close())

		infos, err := r.ListVTEs(ctx)
		require.NoError(t, err)
		require.Len(t, infos, 1)
		assert.Equal(t, id, infos[0].ID)
		assert.Zero(t, infos[0].Attachments)
		assert.False(t, infos[0].Complete)

		// The emulator is still attachable, which is only possible
		// while its pty and process are alive.
		again, err := r.AttachVTE(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, id, again.ID())
	})

	t.Run("delete terminates and forgets the canonical vte", func(t *testing.T) {
		r := newTestVTERegistry(t)
		v, err := r.CreateVTE(ctx, workspace.CreateVTEOptions{
			Width: 20, Height: 10,
		})
		require.NoError(t, err)
		id := v.ID()

		require.NoError(t, r.DeleteVTE(ctx, id))

		infos, err := r.ListVTEs(ctx)
		require.NoError(t, err)
		assert.Empty(t, infos)

		_, err = r.AttachVTE(ctx, id)
		assert.ErrorIs(t, err, workspace.ErrVTENotFound)

		require.Eventually(t, v.IsComplete, 5*time.Second, 10*time.Millisecond,
			"deleting a canonical vte must stop its process")
	})
}

func TestVTERegistryResizeAuthority(t *testing.T) {
	ctx := context.Background()
	r := newTestVTERegistry(t)

	first, err := r.CreateVTE(ctx, workspace.CreateVTEOptions{
		Width: 20, Height: 10,
	})
	require.NoError(t, err)
	second, err := r.AttachVTE(ctx, first.ID())
	require.NoError(t, err)

	canonicalSize := func() (int, int) {
		infos, err := r.ListVTEs(ctx)
		require.NoError(t, err)
		require.Len(t, infos, 1)
		return infos[0].Width, infos[0].Height
	}

	// The creating attachment holds authority until another one is
	// focused, so the second client clips instead of resizing.
	second.Resize(80, 24)
	w, h := canonicalSize()
	assert.Equal(t, 20, w)
	assert.Equal(t, 10, h)

	first.Resize(40, 12)
	w, h = canonicalSize()
	assert.Equal(t, 40, w)
	assert.Equal(t, 12, h)

	// Focus transfers authority and applies the focused client's size.
	second.OnFocusChange(true)
	w, h = canonicalSize()
	assert.Equal(t, 80, w)
	assert.Equal(t, 24, h)

	first.Resize(30, 8)
	w, h = canonicalSize()
	assert.Equal(t, 80, w)
	assert.Equal(t, 24, h)

	// Authority falls back to a remaining attachment on detach.
	require.NoError(t, second.Close())
	first.Resize(50, 14)
	w, h = canonicalSize()
	assert.Equal(t, 50, w)
	assert.Equal(t, 14, h)
}

// TestVTERegistryDetachRestoresTheHeirsSize pins that authority moving
// on detach takes effect at once. A client only asks for a size when
// its own window changes, so waiting for the next request would leave
// the last client rendering at the departed one's size indefinitely.
func TestVTERegistryDetachRestoresTheHeirsSize(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// heirSize is what the surviving client last asked for, or
		// zero when it never asked.
		heirSize     [2]int
		wantW, wantH int
	}{
		{
			name:     "the survivor's last request is restored",
			heirSize: [2]int{30, 8},
			wantW:    30, wantH: 8,
		},
		{
			name:  "a survivor that never asked keeps the size in force",
			wantW: 80, wantH: 24,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestVTERegistry(t)
			heir, err := r.CreateVTE(ctx, workspace.CreateVTEOptions{
				Width: 20, Height: 10,
			})
			require.NoError(t, err)
			owner, err := r.AttachVTE(ctx, heir.ID())
			require.NoError(t, err)

			canonicalSize := func() (int, int) {
				infos, err := r.ListVTEs(ctx)
				require.NoError(t, err)
				require.Len(t, infos, 1)
				return infos[0].Width, infos[0].Height
			}

			owner.OnFocusChange(true)
			owner.Resize(80, 24)
			if tc.heirSize != [2]int{} {
				heir.Resize(tc.heirSize[0], tc.heirSize[1])
			}
			w, h := canonicalSize()
			require.Equal(t, 80, w)
			require.Equal(t, 24, h)

			require.NoError(t, owner.Close())
			w, h = canonicalSize()
			assert.Equal(t, tc.wantW, w)
			assert.Equal(t, tc.wantH, h)
		})
	}
}
