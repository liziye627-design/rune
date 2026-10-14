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
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
	"unstable.build/rune/internal/workspace/workspacetest"
)

func TestClassifyAttachError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want vtewire.ErrCode
	}{
		{
			name: "a confirmed missing terminal",
			err:  fmt.Errorf("attach: %w", workspace.ErrVTENotFound),
			want: vtewire.CodeNotFound,
		},
		{
			name: "a scheme without terminals",
			err:  fmt.Errorf("attach: %w", workspace.ErrVTEUnsupported),
			want: vtewire.CodeUnsupported,
		},
		{
			name: "anything else leaves the terminal's existence unknown",
			err:  errors.New("resolve workspace: permission denied"),
			want: vtewire.CodeUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, message := classifyAttachError(tc.err)
			assert.Equal(t, tc.want, code)
			assert.Equal(t, tc.err.Error(), message)
		})
	}
}

// TestHeadlessVTEHostIsInert pins that a server-side emulator reports
// nothing: bells, notifications and titles belong to the clients.
func TestHeadlessVTEHostIsInert(t *testing.T) {
	t.Parallel()
	h := headlessVTEHost{}

	assert.NoError(t, h.PublishEvent(term.Event{}))
	id, err := h.Notify(browserapi.LevelInfo, "bell")
	require.NoError(t, err)
	assert.Empty(t, id)
	id, err = h.NotifyOnce(browserapi.LevelInfo, "bell")
	require.NoError(t, err)
	assert.Empty(t, id)
	assert.NoError(t, h.UpdateNotificationProgress("id", "msg", 1, 2))
	tab, err := h.Tab(workspaceapi.URI{}, 't', "title", nil)
	require.NoError(t, err)
	assert.Nil(t, tab)
	assert.NoError(t, h.SetTabName(workspaceapi.URI{}, "title", term.Attributes{}))
	assert.False(t, h.OnTabExit(workspaceapi.URI{}))
}

func TestNewVTERegistryRequiresAFactory(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t, "vtescheme: VTEFactory is required",
		func() { NewVTERegistry(nil) })
}

// TestServerRefusesWhatItCannotServe pins what a client is told when
// its attachment cannot be served: only a confirmed missing terminal
// is reported as one, since that alone lets it recreate the terminal.
func TestServerRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		resolve  error
		create   error
		hello    vtewire.Hello
		wantCode vtewire.ErrCode
		wantMsg  string
	}{
		{
			name:     "a workspace it cannot resolve",
			resolve:  errors.New("transport down"),
			hello:    vtewire.Hello{Root: "elsewhere", Width: 20, Height: 10},
			wantCode: vtewire.CodeUnknown,
			wantMsg:  `resolve workspace "elsewhere": transport down`,
		},
		{
			name:     "a terminal it cannot create",
			create:   errors.New("out of ptys"),
			hello:    vtewire.Hello{Width: 20, Height: 10},
			wantCode: vtewire.CodeUnknown,
			wantMsg:  "out of ptys",
		},
		{
			name:     "a terminal it does not have",
			hello:    vtewire.Hello{VTEID: string(workspace.NewVTEID())},
			wantCode: vtewire.CodeNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := &resolvingScheme{
				NopScheme: &workspacetest.NopScheme{},
				resolve: func(path string) (workspaceapi.URI, error) {
					return workspaceapi.URI{}, tc.resolve
				},
			}
			s := newServer(inner, func(string, workspace.CreateVTEOptions, func() (int, int)) (*vte.Handler, error) {
				return nil, tc.create
			})

			client, host := net.Pipe()
			t.Cleanup(func() { _ = client.Close() })
			served := make(chan error, 1)
			go debug.CapturePanicReport(func() {
				served <- s.Attach(context.Background(), host)
			})

			_, err := vtewire.Dial(vtewire.NewConn(client), &tc.hello)
			var wireErr *vtewire.Error
			require.ErrorAs(t, err, &wireErr)
			assert.Equal(t, tc.wantCode, wireErr.Code)
			assert.Contains(t, wireErr.Message, tc.wantMsg)
			assert.Error(t, <-served, "the server reports what it refused")
		})
	}
}

func TestServerAttachEndsWithAClientThatHangsUp(t *testing.T) {
	t.Parallel()
	s := newServer(&workspacetest.NopScheme{},
		func(string, workspace.CreateVTEOptions, func() (int, int)) (*vte.Handler, error) {
			t.Error("nothing may be created without a handshake")
			return nil, errors.New("unreachable")
		})

	client, host := net.Pipe()
	require.NoError(t, client.Close())
	assert.Error(t, s.Attach(context.Background(), host))
}

// TestServeStopsWithItsListener pins that closing the attachment
// listener is a clean stop only when the server was asked to stop: a
// listener that dies under a running server is its caller's to report.
func TestServeStopsWithItsListener(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		cancel  bool
		wantErr error
	}{
		{name: "asked to stop", cancel: true},
		{name: "its listener died", wantErr: net.ErrClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lis, err := net.Listen("unix", workspacetest.ShortTempDir(t)+"/vte.sock")
			require.NoError(t, err)
			s := newServer(&workspacetest.NopScheme{},
				func(string, workspace.CreateVTEOptions, func() (int, int)) (*vte.Handler, error) {
					return nil, errors.New("unused")
				})

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			served := make(chan error, 1)
			go debug.CapturePanicReport(func() { served <- s.Serve(ctx, lis) })
			if tc.cancel {
				cancel()
			}
			require.NoError(t, lis.Close())

			select {
			case err := <-served:
				if tc.wantErr == nil {
					assert.NoError(t, err)
				} else {
					assert.ErrorIs(t, err, tc.wantErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve kept running without a listener")
			}
		})
	}
}

func TestServerChrootToAnUnresolvableWorkspace(t *testing.T) {
	t.Parallel()
	s := newServer(&resolvingScheme{
		NopScheme: &workspacetest.NopScheme{},
		resolve: func(string) (workspaceapi.URI, error) {
			return workspaceapi.URI{}, errors.New("transport down")
		},
	}, func(string, workspace.CreateVTEOptions, func() (int, int)) (*vte.Handler, error) {
		return nil, errors.New("unused")
	})

	_, err := s.Chroot("alpha")
	require.Error(t, err)
	assert.Equal(t, `resolve workspace "alpha": transport down`, err.Error())
}

// TestWirePtyIsNotAFile pins that a replica's pty refuses the file
// operations a byte stream cannot answer instead of pretending to.
func TestWirePtyIsNotAFile(t *testing.T) {
	t.Parallel()
	p := &wirePty{name: "vte://id"}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Stat", func() error { _, err := p.Stat(); return err }},
		{"Truncate", func() error { return p.Truncate(0) }},
		{"Seek", func() error { _, err := p.Seek(0, 0); return err }},
		{"ReadAt", func() error { _, err := p.ReadAt(nil, 0); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.ErrorIs(t, tc.call(), errNotAFile)
		})
	}
	assert.Equal(t, "vte://id", p.Name())
	assert.NoError(t, p.Sync(), "there is nothing to flush")
	assert.Zero(t, p.Fd())
}
