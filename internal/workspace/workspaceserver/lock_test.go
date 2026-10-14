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

package workspaceserver

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireLock(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, dataDir string)
	}{
		{
			name: "a second holder is refused naming the first",
			run: func(t *testing.T, dataDir string) {
				lock, err := AcquireLock(dataDir)
				require.NoError(t, err)
				t.Cleanup(func() { _ = lock.Release() })

				_, err = AcquireLock(dataDir)
				require.ErrorIs(t, err, ErrLocked)
				assert.Equal(t,
					fmt.Sprintf("there's another instance of Rune holding the lock "+
						"on %s (pid %d)", LockPath(dataDir), os.Getpid()),
					err.Error())
			},
		},
		{
			name: "a released lock can be taken again",
			run: func(t *testing.T, dataDir string) {
				lock, err := AcquireLock(dataDir)
				require.NoError(t, err)
				require.NoError(t, lock.Release())

				again, err := AcquireLock(dataDir)
				require.NoError(t, err)
				require.NoError(t, again.Release())
				assert.FileExists(t, LockPath(dataDir),
					"the lock file outlives its holder so racing starters "+
						"always contend on the same inode")
			},
		},
		{
			name: "data directories do not share a lock",
			run: func(t *testing.T, dataDir string) {
				lock, err := AcquireLock(dataDir)
				require.NoError(t, err)
				t.Cleanup(func() { _ = lock.Release() })

				other, err := AcquireLock(filepath.Join(dataDir, "other"))
				require.NoError(t, err)
				require.NoError(t, other.Release())
			},
		},
		{
			name: "the lock directory stays private",
			run: func(t *testing.T, dataDir string) {
				lock, err := AcquireLock(dataDir)
				require.NoError(t, err)
				t.Cleanup(func() { _ = lock.Release() })

				dir, err := os.Stat(SocketDir(dataDir))
				require.NoError(t, err)
				assert.Equal(t, dirPerm, dir.Mode().Perm())
				file, err := os.Stat(LockPath(dataDir))
				require.NoError(t, err)
				assert.Equal(t, socketPerm, file.Mode().Perm())
			},
		},
		{
			name: "a lock left without a pid is still reported as held",
			run: func(t *testing.T, dataDir string) {
				lock, err := AcquireLock(dataDir)
				require.NoError(t, err)
				t.Cleanup(func() { _ = lock.Release() })
				require.NoError(t, os.WriteFile(LockPath(dataDir), nil, socketPerm))

				_, err = AcquireLock(dataDir)
				require.ErrorIs(t, err, ErrLocked)
				assert.Equal(t,
					"there's another instance of Rune holding the lock on "+
						LockPath(dataDir), err.Error())
			},
		},
		{
			name: "an unusable data directory is an error, not a held lock",
			run: func(t *testing.T, dataDir string) {
				file := filepath.Join(dataDir, "file")
				require.NoError(t, os.WriteFile(file, nil, 0o600))

				_, err := AcquireLock(file)
				require.Error(t, err)
				assert.NotErrorIs(t, err, ErrLocked)
				assert.Contains(t, err.Error(), "create lock dir")
			},
		},
		{
			name: "an unusable lock file is an error, not a held lock",
			run: func(t *testing.T, dataDir string) {
				require.NoError(t, os.MkdirAll(LockPath(dataDir), 0o700))

				_, err := AcquireLock(dataDir)
				require.Error(t, err)
				assert.NotErrorIs(t, err, ErrLocked)
				assert.Contains(t, err.Error(), "open lock")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t, t.TempDir())
		})
	}
}
