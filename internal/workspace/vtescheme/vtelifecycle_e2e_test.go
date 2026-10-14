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
	"unstable.build/rune/internal/workspace/vtewire"
)

// TestVTELifecycleOverRPC pins that which terminals exist, and
// terminating one, survive the transport — including the typed
// not-found that authorizes recreating a terminal from a snapshot.
func TestVTELifecycleOverRPC(t *testing.T) {
	ctx := context.Background()

	h := newWireHarness(t)
	c := setupVTELifecycle(t, h)

	first := h.attachClient(t, &vtewire.Hello{Width: 20, Height: 10})
	second := h.attachClient(t, &vtewire.Hello{
		VTEID: string(first.ID()), Width: 20, Height: 10,
	})

	infos, err := c.ListVTEs(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, first.ID(), infos[0].ID)
	assert.Equal(t, 2, infos[0].Attachments)

	assert.ErrorIs(t, c.DeleteVTE(ctx, workspace.NewVTEID()),
		workspace.ErrVTENotFound)

	require.NoError(t, c.DeleteVTE(ctx, first.ID()))
	infos, err = c.ListVTEs(ctx)
	require.NoError(t, err)
	assert.Empty(t, infos)

	require.Eventually(t, second.IsComplete, 5*time.Second,
		10*time.Millisecond,
		"deleting the canonical vte must stop its process")
}
