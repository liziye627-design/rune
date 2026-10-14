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

package workspaced

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// TestSchemeOwnsTheServersTerminals pins that the scheme a server
// exposes for serving elsewhere reaches the same terminals its socket
// clients do.
func TestSchemeOwnsTheServersTerminals(t *testing.T) {
	t.Parallel()
	dataDir := workspacetest.ShortTempDir(t)
	root := t.TempDir()
	s := startAt(t, dataDir, root)
	client := newClient(t, root, dataDir)

	ctx := vteCtx(context.Background())
	v, err := client.CreateVTE(ctx, workspace.CreateVTEOptions{
		CommandAndArgs: []string{"cat"}, Width: 20, Height: 10,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })

	view, err := s.Scheme().Chroot(root)
	require.NoError(t, err)
	infos, err := view.(workspace.VTEOwner).ListVTEs(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, v.ID(), infos[0].ID)
}
