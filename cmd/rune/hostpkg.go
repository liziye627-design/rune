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

	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/blue/release"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/storageapi"
	"unstable.build/rune/internal/ide"
	"unstable.build/rune/internal/ide/idepkg"
	"unstable.build/rune/internal/ide/idepkg/pkgtrust"
)

// newHostPackageManager builds the manager a host with no editor of
// its own serves to the clients installing packages on it. applyEnv
// re-applies the user config's gui.env to this process after a package
// changed it, so commands started afterwards see the package's
// toolchain. The caller must Close the returned storage.
func newHostPackageManager(
	rootStorage storageapi.Service, releases release.Manager,
	scheme schemeapi.Scheme, applyEnv func(),
) (*idepkg.Manager, storageapi.Service) {
	trust := pkgtrust.NewStore(*flagDataPath, trustKeyringFetcher())
	mgr, storage := idepkg.NewProvisioningManager(
		rootStorage, releases, scheme,
		*flagDataPath, *flagConfigPath, resolveRemoteEditorMode(),
		defaultConfigTree, trust,
		idepkg.WithAfterConfigMerge(func(
			event idepkg.ConfigMergeEvent,
		) (idepkg.ConfigMergeResult, error) {
			if !event.TouchesPath("gui", "env") {
				return idepkg.ConfigMergeResult{}, nil
			}
			applyEnv()
			return idepkg.ConfigMergeResult{LivePaths: [][]string{{"gui", "env"}}}, nil
		}))
	if err := mgr.Reconcile(context.Background()); err != nil {
		log.Warnf("packages: clean up incomplete installs: %v", err)
	}
	return mgr, storage
}

// defaultConfigTree is the config base package .star configs are read
// against; it must match the editor's.
func defaultConfigTree() map[string]any {
	tree, err := ide.DefaultConfigTree(runeDefaultConfig())
	if err != nil {
		log.Warnf("packages: decode default config tree: %v", err)
		return nil
	}
	return tree
}
