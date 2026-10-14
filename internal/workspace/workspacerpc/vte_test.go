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

package workspacerpc

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// TestVTELifecycleIsScopedToAWorkspace pins how one server keeps every
// workspace it hosts to its own terminals: a request naming another
// workspace is served by the scheme chrooted to it.
func TestVTELifecycleIsScopedToAWorkspace(t *testing.T) {
	t.Parallel()
	own := []workspace.VTEInfo{{ID: "own", Title: "own"}}
	nested := []workspace.VTEInfo{{ID: "nested", Title: "nested"}}

	for _, tc := range []struct {
		name       string
		root       string
		chrootErr  error
		listErr    error
		wantIDs    []workspace.VTEID
		wantChroot []string
		wantIs     error
		wantMsg    string
	}{
		{
			name:    "no root is the server's own workspace",
			wantIDs: []workspace.VTEID{"own"},
		},
		{
			name:    "the server's own root is not chrooted",
			root:    "/ws/./",
			wantIDs: []workspace.VTEID{"own"},
		},
		{
			name:       "another root is served by its chroot",
			root:       "/ws/nested",
			wantIDs:    []workspace.VTEID{"nested"},
			wantChroot: []string{"/ws/nested"},
		},
		{
			name:       "a chroot that owns no terminals is unsupported",
			root:       "/ws/plain",
			wantChroot: []string{"/ws/plain"},
			wantIs:     workspace.ErrVTEUnsupported,
		},
		{
			name:       "a workspace that cannot be chrooted fails the call",
			root:       "/ws/missing",
			chrootErr:  errors.New("no such directory"),
			wantChroot: []string{"/ws/missing"},
			wantMsg:    "no such directory",
		},
		{
			name:    "a workspace that cannot list its terminals fails the call",
			listErr: errors.New("registry unavailable"),
			wantMsg: "registry unavailable",
		},
		{
			name:    "a workspace without terminals stays typed across the wire",
			listErr: workspace.ErrVTEUnsupported,
			wantIs:  workspace.ErrVTEUnsupported,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newVTEScheme("/ws", own)
			s.chrootErr = tc.chrootErr
			s.listErr = tc.listErr
			s.chroots["/ws/nested"] = newVTEScheme("/ws/nested", nested)
			client := NewVTEClient(serveVTEs(t, s), tc.root)

			infos, err := client.ListVTEs(context.Background())
			switch {
			case tc.wantIs != nil:
				assert.ErrorIs(t, err, tc.wantIs)
			case tc.wantMsg != "":
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantMsg)
				assert.NotErrorIs(t, err, workspace.ErrVTENotFound,
					"only a confirmed missing terminal may be reported as one")
			default:
				require.NoError(t, err)
				ids := make([]workspace.VTEID, 0, len(infos))
				for _, info := range infos {
					ids = append(ids, info.ID)
				}
				assert.Equal(t, tc.wantIDs, ids)
			}
			assert.Equal(t, tc.wantChroot, s.chrooted())
		})
	}
}

func TestVTEDelete(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		root        string
		deleteErr   error
		wantDeleted map[string][]workspace.VTEID
		wantIs      error
	}{
		{
			name:        "deletes in the server's own workspace",
			wantDeleted: map[string][]workspace.VTEID{"/ws": {"id"}},
		},
		{
			name:        "deletes in the workspace the request names",
			root:        "/ws/nested",
			wantDeleted: map[string][]workspace.VTEID{"/ws/nested": {"id"}},
		},
		{
			name:        "a missing terminal stays typed across the wire",
			deleteErr:   workspace.ErrVTENotFound,
			wantDeleted: map[string][]workspace.VTEID{"/ws": {"id"}},
			wantIs:      workspace.ErrVTENotFound,
		},
		{
			name:        "a workspace without terminals is unsupported",
			deleteErr:   workspace.ErrVTEUnsupported,
			wantDeleted: map[string][]workspace.VTEID{"/ws": {"id"}},
			wantIs:      workspace.ErrVTEUnsupported,
		},
		{
			name:   "a workspace that owns no terminals is unsupported",
			root:   "/ws/plain",
			wantIs: workspace.ErrVTEUnsupported,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newVTEScheme("/ws", nil)
			s.deleteErr = tc.deleteErr
			nested := newVTEScheme("/ws/nested", nil)
			s.chroots["/ws/nested"] = nested
			client := NewVTEClient(serveVTEs(t, s), tc.root)

			err := client.DeleteVTE(context.Background(), "id")
			if tc.wantIs != nil {
				assert.ErrorIs(t, err, tc.wantIs)
			} else {
				require.NoError(t, err)
			}
			deleted := map[string][]workspace.VTEID{}
			for _, scheme := range []*vteScheme{s, nested} {
				if ids := scheme.deletedIDs(); len(ids) > 0 {
					deleted[scheme.root] = ids
				}
			}
			if tc.wantDeleted == nil {
				tc.wantDeleted = map[string][]workspace.VTEID{}
			}
			assert.Equal(t, tc.wantDeleted, deleted)
		})
	}
}

// TestRegisterServicesOffersTerminalsOnlyWhenOwned pins that a server
// whose scheme owns no terminals answers like one that never heard of
// them, which clients map to ErrVTEUnsupported.
func TestRegisterServicesOffersTerminalsOnlyWhenOwned(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		scheme schemeapi.Scheme
		wantIs error
	}{
		{name: "owner", scheme: newVTEScheme("/ws", nil)},
		{
			name:   "not an owner",
			scheme: &workspacetest.NopScheme{},
			wantIs: workspace.ErrVTEUnsupported,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			grpcServer := grpc.NewServer()
			server := NewServer(tc.scheme, CommandAuthorizerFunc(
				func(context.Context, workspaceapi.Cmd) error { return nil }))
			t.Cleanup(func() { _ = server.Stop() })
			RegisterServices(grpcServer, server)

			_, err := NewVTEClient(serve(t, grpcServer), "").
				ListVTEs(context.Background())
			if tc.wantIs != nil {
				assert.ErrorIs(t, err, tc.wantIs)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewVTEServerRequiresAScheme(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { NewVTEServer(nil) })
}

func serveVTEs(t *testing.T, s schemeapi.Scheme) *grpc.ClientConn {
	t.Helper()
	grpcServer := grpc.NewServer()
	server := NewServer(s, CommandAuthorizerFunc(
		func(context.Context, workspaceapi.Cmd) error { return nil }))
	t.Cleanup(func() { _ = server.Stop() })
	RegisterServices(grpcServer, server)
	return serve(t, grpcServer)
}

func serve(t *testing.T, grpcServer *grpc.Server) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go debug.CapturePanicReport(func() { _ = grpcServer.Serve(lis) })
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// vteScheme owns a fixed set of terminals and reaches other workspaces
// through the chroots it was given; any other chroot owns none.
type vteScheme struct {
	*workspacetest.NopScheme
	root      string
	infos     []workspace.VTEInfo
	chroots   map[string]schemeapi.Scheme
	chrootErr error
	listErr   error
	deleteErr error

	mu       sync.Mutex
	chrootTo []string
	deleted  []workspace.VTEID
}

var _ workspace.VTEOwner = (*vteScheme)(nil)

func newVTEScheme(root string, infos []workspace.VTEInfo) *vteScheme {
	return &vteScheme{
		NopScheme: &workspacetest.NopScheme{},
		root:      root,
		infos:     infos,
		chroots:   make(map[string]schemeapi.Scheme),
	}
}

func (s *vteScheme) Root() string { return s.root }

func (s *vteScheme) Chroot(path string) (schemeapi.Scheme, error) {
	s.mu.Lock()
	s.chrootTo = append(s.chrootTo, path)
	s.mu.Unlock()
	if s.chrootErr != nil {
		return nil, s.chrootErr
	}
	if chroot, ok := s.chroots[path]; ok {
		return chroot, nil
	}
	return &workspacetest.NopScheme{}, nil
}

func (s *vteScheme) chrooted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chrootTo
}

func (s *vteScheme) CreateVTE(
	context.Context, workspace.CreateVTEOptions,
) (workspace.VTE, error) {
	return nil, workspace.ErrVTEUnsupported
}

func (s *vteScheme) ListVTEs(context.Context) ([]workspace.VTEInfo, error) {
	return s.infos, s.listErr
}

func (s *vteScheme) AttachVTE(
	context.Context, workspace.VTEID,
) (workspace.VTE, error) {
	return nil, workspace.ErrVTEUnsupported
}

func (s *vteScheme) DeleteVTE(_ context.Context, id workspace.VTEID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return s.deleteErr
}

func (s *vteScheme) deletedIDs() []workspace.VTEID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted
}
