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

package workspacetest

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// ShortTempDir returns a directory removed when t ends, short enough
// that unix sockets inside it stay within the sockaddr_un path limit,
// which the default per-test temp dir already exceeds on macOS.
func ShortTempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rune")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
