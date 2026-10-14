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

package workspaced

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspacetest"
)

func TestReadStatus(t *testing.T) {
	t.Parallel()

	want := Status{
		Schema:    StatusSchema,
		PID:       42,
		StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Workspaces: []WorkspaceStatus{{
			Root: "/home/me/proj",
			VTEs: []VTEStatus{{
				ID:             "vte-1",
				Title:          "zsh",
				CommandAndArgs: []string{"zsh", "-l"},
				Attachments:    1,
				CreatedAt:      time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
			}},
		}},
		Network: NetworkStatus{
			State:     "Running",
			Hostname:  "laptop",
			LoginName: "me@example.com",
			Peers:     []PeerStatus{{Hostname: "desk", DNSName: "desk.example.ts.net", OS: "linux", Online: true}},
		},
	}
	for _, tc := range []struct {
		name    string
		write   func(t *testing.T, path string)
		want    Status
		wantErr error
		wantMsg string
	}{
		{
			name: "what a server wrote",
			write: func(t *testing.T, path string) {
				data, err := json.Marshal(want)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o600))
			},
			want: want,
		},
		{
			name:    "no server",
			write:   func(*testing.T, string) {},
			wantErr: os.ErrNotExist,
		},
		{
			name: "another schema",
			write: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte(`{"schema": 2}`), 0o600))
			},
			wantMsg: "schema 2, want 1",
		},
		{
			name: "not json",
			write: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte(`{"schema": `), 0o600))
			},
			wantMsg: "decode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Dir(StatusPath(dataDir)), 0o700))
			tc.write(t, StatusPath(dataDir))

			got, err := ReadStatus(dataDir)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			if tc.wantMsg != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantMsg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestStatusWriter pins that a burst of changes is one write of the
// latest state, that a reader never sees a partial file, and that the
// file does not outlive its writer.
func TestStatusWriter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")

	var collected atomic.Int32
	var pid atomic.Int32
	w := newStatusWriter(path, 50*time.Millisecond, func() Status {
		collected.Add(1)
		return Status{Schema: StatusSchema, PID: int(pid.Load())}
	})
	t.Cleanup(w.Close)

	pid.Store(1)
	for range 10 {
		w.Notify()
	}
	require.Eventually(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, 5*time.Second, 5*time.Millisecond)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var got Status
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, 1, got.PID)
	assert.Equal(t, int32(1), collected.Load(), "a burst of changes is one write")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing but the status is left behind")
	assert.Equal(t, "status.json", entries[0].Name())

	pid.Store(2)
	w.Notify()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var got Status
		return json.Unmarshal(data, &got) == nil && got.PID == 2
	}, 5*time.Second, 5*time.Millisecond, "a later change is written")

	w.Close()
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist, "a closed writer removes its status")
	w.Notify()
	w.Close()
}

func TestVTEStatusIsOrderedByRoot(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got := vteStatus(map[string][]workspace.VTEInfo{
		"/b": {{ID: "2", Title: "two", Attachments: 1, CreatedAt: created}},
		"/a": {{ID: "1", CommandAndArgs: []string{"cat"}, Complete: true, CreatedAt: created}},
	})
	assert.Equal(t, []WorkspaceStatus{
		{Root: "/a", VTEs: []VTEStatus{{
			ID: "1", CommandAndArgs: []string{"cat"}, Complete: true, CreatedAt: created,
		}}},
		{Root: "/b", VTEs: []VTEStatus{{
			ID: "2", Title: "two", Attachments: 1, CreatedAt: created,
		}}},
	}, got)
	assert.Empty(t, vteStatus(nil))
	assert.NotNil(t, vteStatus(nil), "no workspaces is an empty list, not null")
}

// TestServerPublishesItsStatus pins that a running server is
// observable from its status file alone: who it is, what the hosting
// process reports about the network, and nothing once it stopped.
func TestServerPublishesItsStatus(t *testing.T) {
	t.Parallel()
	dataDir := workspacetest.ShortTempDir(t)
	var state atomic.Value
	state.Store("NeedsLogin")
	s, err := Start(Options{
		DataDir: dataDir,
		Scheme:  newScheme(t, t.TempDir()),
		Network: func() NetworkStatus {
			return NetworkStatus{State: state.Load().(string), Hostname: "laptop"}
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var got Status
	require.Eventually(t, func() bool {
		got, err = ReadStatus(dataDir)
		return err == nil
	}, 5*time.Second, 5*time.Millisecond, "a started server publishes its status")
	assert.Equal(t, os.Getpid(), got.PID)
	assert.WithinDuration(t, time.Now(), got.StartedAt, time.Minute)
	// Readers iterate the lists without checking for null.
	assert.Equal(t, NetworkStatus{
		State: "NeedsLogin", Hostname: "laptop", Peers: []PeerStatus{},
	}, got.Network)
	assert.NotNil(t, got.Workspaces)
	assert.Empty(t, got.Workspaces)

	state.Store("Running")
	s.StatusChanged()
	require.Eventually(t, func() bool {
		got, err = ReadStatus(dataDir)
		return err == nil && got.Network.State == "Running"
	}, 5*time.Second, 5*time.Millisecond, "a network change is published")

	require.NoError(t, s.Close())
	_, err = ReadStatus(dataDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "a stopped server publishes nothing")
}
