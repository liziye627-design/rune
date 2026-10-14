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

package vtescheme

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/workspace/workspacetest"
)

func TestRootedExecutor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		dir     string
		wantDir string
	}{
		{name: "starts in the workspace by default", wantDir: "/ws"},
		{name: "an explicit directory wins", dir: "/elsewhere", wantDir: "/elsewhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got workspaceapi.Cmd
			inner := &workspacetest.NopScheme{
				StartCommandFunc: func(_ context.Context, cmd workspaceapi.Cmd) (workspaceapi.Pid, error) {
					got = cmd
					return 42, nil
				},
			}
			pid, err := rootedExecutor{Executor: inner, root: "/ws"}.StartCommand(
				context.Background(), workspaceapi.Cmd{Path: "sh", Dir: tc.dir})
			require.NoError(t, err)
			assert.Equal(t, workspaceapi.Pid(42), pid)
			assert.Equal(t, tc.wantDir, got.Dir)
			assert.Equal(t, "sh", got.Path)
		})
	}
}
