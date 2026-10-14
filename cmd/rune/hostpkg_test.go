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

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/blue/release"
	"unstable.build/rune/internal/ide/idepkg/gitpkg"
)

func TestHostPackageManagerInstallsPackage(t *testing.T) {
	const pkgID = "github.com/unstablebuild/test-extension"
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "config.yaml"),
		[]byte("gui:\n  env:\n    RUNE_TEST_HOST_PKG: \"1\"\n"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(".")
	require.NoError(t, err)
	sha, err := wt.Commit("fixture", &git.CommitOptions{Author: &object.Signature{
		Name: "fixture", Email: "fixture@example.com", When: time.Now(),
	}})
	require.NoError(t, err)
	version := release.Version(sha.String()[:12])

	dataDir := t.TempDir()
	setFlagForTest(t, flagDataPath, dataDir)
	setFlagForTest(t, flagConfigPath, filepath.Join(dataDir, "config.yaml"))
	setFlagForTest(t, flagHTTPAddress, "http://127.0.0.1:0")
	cwd, _ := newTestFileScheme(t, t.TempDir())
	releases := newRemoteReleaseManager(gitpkg.WithRemoteURL(func(id string) string {
		assert.Equal(t, pkgID, id)
		return repoDir
	}))
	var applied int
	pkgs, storage := newHostPackageManager(newRuneStorage(dataDir), releases,
		cwd, func() { applied++ })
	t.Cleanup(func() { _ = storage.Close() })

	ctx := context.Background()
	require.NoError(t, pkgs.InstallPackageVersion(ctx, pkgID, version, nil))

	inUse, ok, err := pkgs.PackageVersionInUse(ctx, pkgID)
	require.NoError(t, err)
	require.True(t, ok, "an installed package is activated")
	assert.Equal(t, version, inUse)
	target, err := os.Readlink(filepath.Join(dataDir, "lib", filepath.FromSlash(pkgID)))
	require.NoError(t, err)
	assert.Equal(t,
		filepath.Join(dataDir, "pkg", filepath.FromSlash(pkgID), string(version)), target)
	assert.Equal(t, 1, applied, "the package's gui.env is applied to this process")
}
