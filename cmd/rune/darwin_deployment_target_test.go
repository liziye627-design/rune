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
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinAppBundlesPinMinimumOSVersion(t *testing.T) {
	repoRoot := repoRootDir(t)
	makefile := readFileString(t, filepath.Join(repoRoot, "cmd", "rune", "Makefile"))

	cases := []struct {
		name      string
		floorVar  string
		plistPath string
	}{
		{
			name:      "arm64",
			floorVar:  "DARWIN_ARM64_MIN_MACOS",
			plistPath: filepath.Join(repoRoot, "extra", "osx", "Rune.app", "Contents", "Info.plist"),
		},
		{
			name:      "amd64",
			floorVar:  "DARWIN_AMD64_MIN_MACOS",
			plistPath: filepath.Join(repoRoot, "extra", "osx", "Info.amd64.plist"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			floor := makefileVar(t, makefile, tc.floorVar)
			plist := readFileString(t, tc.plistPath)
			got := plistString(t, plist, "LSMinimumSystemVersion")
			require.Equalf(t, floor, got,
				"%s LSMinimumSystemVersion must match Makefile %s; "+
					"otherwise the bundle inherits the build SDK's minimum OS",
				tc.name, tc.floorVar)
		})
	}
}

// TestDarwinDockTilePlugInMatchesTheApp guards the Dock tile plug-in
// against drifting from the app that ships it: the Dock only loads a
// plug-in the app's plist names, from the app's PlugIns directory, and
// refuses one built for a newer macOS than the app. One plug-in serves
// both app builds, so its floor must be each app's floor, and the
// Makefile must build it at that floor.
func TestDarwinDockTilePlugInMatchesTheApp(t *testing.T) {
	repoRoot := repoRootDir(t)
	makefile := readFileString(t, filepath.Join(repoRoot, "cmd", "rune", "Makefile"))
	plugInPlist := readFileString(t,
		filepath.Join(repoRoot, "extra", "osx", "RuneDockTile", "Info.plist"))
	plugInFloor := plistString(t, plugInPlist, "LSMinimumSystemVersion")
	require.Equal(t, "RuneDockTilePlugIn", plistString(t, plugInPlist, "NSPrincipalClass"))
	require.Equal(t, "BNDL", plistString(t, plugInPlist, "CFBundlePackageType"))

	bundleName := makefileVar(t, makefile, "DOCK_TILE_NAME")
	cases := []struct {
		name      string
		floorVar  string
		plistPath string
		ruleVar   string
	}{
		{
			name:      "arm64",
			floorVar:  "DARWIN_ARM64_MIN_MACOS",
			plistPath: filepath.Join(repoRoot, "extra", "osx", "Rune.app", "Contents", "Info.plist"),
			ruleVar:   "dock-tile-arm64: DOCK_TILE_MIN_MACOS",
		},
		{
			name:      "amd64",
			floorVar:  "DARWIN_AMD64_MIN_MACOS",
			plistPath: filepath.Join(repoRoot, "extra", "osx", "Info.amd64.plist"),
			ruleVar:   "dock-tile-amd64: DOCK_TILE_MIN_MACOS",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			appPlist := readFileString(t, tc.plistPath)
			require.Equalf(t, bundleName, plistString(t, appPlist, "NSDockTilePlugIn"),
				"%s app plist must name the plug-in bundle the Makefile ships", tc.name)
			floor := makefileVar(t, makefile, tc.floorVar)
			require.Equalf(t, floor, plugInFloor,
				"plug-in LSMinimumSystemVersion must match Makefile %s", tc.floorVar)
			require.Equalf(t, "$("+tc.floorVar+")", makefileVar(t, makefile, tc.ruleVar),
				"the Makefile must build the %s plug-in at the app's floor", tc.name)
		})
	}
}

func repoRootDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read %s", path)
	return string(b)
}

// makefileVar returns the value assigned to name via `name ?= value`,
// `name := value` or `name = value`, ignoring surrounding whitespace.
func makefileVar(t *testing.T, makefile, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*[?:]?=\s*(\S+)`)
	m := re.FindStringSubmatch(makefile)
	require.Lenf(t, m, 2, "could not find %s assignment in Makefile", name)
	return m[1]
}

// plistString returns the <string> value following the given <key> in
// a property list document.
func plistString(t *testing.T, plist, key string) string {
	t.Helper()
	re := regexp.MustCompile(
		`(?s)<key>` + regexp.QuoteMeta(key) + `</key>\s*<string>(.*?)</string>`)
	m := re.FindStringSubmatch(plist)
	require.Lenf(t, m, 2, "could not find <key>%s</key> in plist", key)
	return m[1]
}
