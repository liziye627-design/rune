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
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/term/programstatus"
)

// NopVTEHost stands in for the browser a terminal client reports to,
// for tests that attach to terminals without one: it publishes,
// notifies and opens tabs nowhere.
type NopVTEHost struct{}

// PublishEvent satisfies browser.EventPublisher.
func (NopVTEHost) PublishEvent(term.Event) error { return nil }

// Notify satisfies browser.Notifications.
func (NopVTEHost) Notify(
	browserapi.NotificationLevel, string, ...any,
) (string, error) {
	return "", nil
}

// NotifyOnce satisfies browser.Notifications.
func (NopVTEHost) NotifyOnce(
	browserapi.NotificationLevel, string, ...any,
) (string, error) {
	return "", nil
}

// UpdateNotificationProgress satisfies browser.Notifications.
func (NopVTEHost) UpdateNotificationProgress(
	string, string, int64, int64,
) error {
	return nil
}

// Tab satisfies browser.TabManager.
func (NopVTEHost) Tab(
	workspaceapi.URI, rune, string, browserapi.Handler,
) (browserapi.Handler, error) {
	return nil, nil
}

// SetTabName satisfies browser.TabManager.
func (NopVTEHost) SetTabName(workspaceapi.URI, string, term.Attributes) error {
	return nil
}

// OnTabExit satisfies browser.TabManager.
func (NopVTEHost) OnTabExit(workspaceapi.URI) bool { return false }

// SetTabStatus satisfies browser.TabManager.
func (NopVTEHost) SetTabStatus(workspaceapi.URI, programstatus.Report) error {
	return nil
}

// EndTabStatus satisfies browser.TabManager.
func (NopVTEHost) EndTabStatus(workspaceapi.URI) error { return nil }
