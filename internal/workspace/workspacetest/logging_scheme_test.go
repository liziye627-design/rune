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
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/workspace"
)

func TestLoggingScheme(t *testing.T) {
	TestWorkspaceSchemeFiles(t, func(t *testing.T) schemeapi.Scheme {
		uri, err := workspaceapi.ParseURI("memory:///")
		require.NoError(t, err)
		log := workspace.LoggingScheme("memory", workspace.NewMemoryScheme)
		scheme, err := log(context.Background(), config.NopConfig(), uri)
		require.NoError(t, err)
		return scheme
	})
}

// TestLoggingSchemeKeepsHostCapabilities pins that tracing a scheme does
// not change what its workspace can do: the manager wraps every scheme
// once trace logging is on, and a wrapper that hid the optional
// capabilities left such a session with no terminals and deaf to
// transport drops.
func TestLoggingSchemeKeepsHostCapabilities(t *testing.T) {
	uri, err := workspaceapi.ParseURI("memory:///")
	require.NoError(t, err)
	disconnect := make(chan struct{})
	for _, tc := range []struct {
		name  string
		inner func(schemeapi.Scheme) schemeapi.Scheme
		check func(*testing.T, workspace.Workspace)
	}{{
		name: "a capable scheme keeps its capabilities",
		inner: func(s schemeapi.Scheme) schemeapi.Scheme {
			return &capableScheme{Scheme: s, disconnect: disconnect}
		},
		check: func(t *testing.T, w workspace.Workspace) {
			ctx := context.Background()
			_, err := w.CreateVTE(ctx, workspace.CreateVTEOptions{})
			require.ErrorIs(t, err, errCapableCreate)
			infos, err := w.ListVTEs(ctx)
			require.NoError(t, err)
			require.Equal(t, []workspace.VTEInfo{{ID: "listed"}}, infos)
			_, err = w.AttachVTE(ctx, "attached")
			require.ErrorIs(t, err, errCapableAttach)
			require.ErrorIs(t, w.DeleteVTE(ctx, "deleted"), errCapableDelete)
			rs, ok := w.(workspace.RemoteScheme)
			require.True(t, ok)
			require.Equal(t, (<-chan struct{})(disconnect), rs.OnDisconnect())
			require.ErrorIs(t, rs.WaitConnected(ctx), errCapableConnect)
			dir, err := w.InstallDataDir(ctx)
			require.NoError(t, err)
			require.Equal(t, "/capable", dir)
		},
	}, {
		name:  "a plain scheme still lacks them",
		inner: func(s schemeapi.Scheme) schemeapi.Scheme { return s },
		check: func(t *testing.T, w workspace.Workspace) {
			ctx := context.Background()
			_, err := w.CreateVTE(ctx, workspace.CreateVTEOptions{})
			require.ErrorIs(t, err, workspace.ErrVTEUnsupported)
			_, err = w.ListVTEs(ctx)
			require.ErrorIs(t, err, workspace.ErrVTEUnsupported)
			_, err = w.AttachVTE(ctx, "attached")
			require.ErrorIs(t, err, workspace.ErrVTEUnsupported)
			require.ErrorIs(t, w.DeleteVTE(ctx, "deleted"), workspace.ErrVTEUnsupported)
			rs, ok := w.(workspace.RemoteScheme)
			require.True(t, ok)
			require.Nil(t, rs.OnDisconnect())
			require.NoError(t, rs.WaitConnected(ctx))
			_, err = w.InstallDataDir(ctx)
			require.ErrorIs(t, err, errors.ErrUnsupported)
		},
	}} {
		for _, traced := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/traced=%t", tc.name, traced), func(t *testing.T) {
				newScheme := func(
					ctx context.Context, cfg config.Config, uri workspaceapi.URI,
				) (schemeapi.Scheme, error) {
					s, err := workspace.NewMemoryScheme(ctx, cfg, uri)
					if err != nil {
						return nil, err
					}
					return tc.inner(s), nil
				}
				if traced {
					newScheme = workspace.LoggingScheme("memory", newScheme)
				}
				scheme, err := newScheme(context.Background(), config.NopConfig(), uri)
				require.NoError(t, err)
				t.Cleanup(func() { _ = scheme.Close() })
				tc.check(t, workspace.NewSchemeWorkspace(uri, scheme, inlineSchedule))
			})
		}
	}
}

var (
	errCapableCreate  = errors.New("created by the wrapped scheme")
	errCapableAttach  = errors.New("attached by the wrapped scheme")
	errCapableDelete  = errors.New("deleted by the wrapped scheme")
	errCapableConnect = errors.New("connected by the wrapped scheme")
)

// capableScheme has every optional host capability, each answering in
// a way only it could.
type capableScheme struct {
	schemeapi.Scheme
	disconnect chan struct{}
}

func (*capableScheme) CreateVTE(
	context.Context, workspace.CreateVTEOptions,
) (workspace.VTE, error) {
	return nil, errCapableCreate
}

func (*capableScheme) ListVTEs(context.Context) ([]workspace.VTEInfo, error) {
	return []workspace.VTEInfo{{ID: "listed"}}, nil
}

func (*capableScheme) AttachVTE(
	context.Context, workspace.VTEID,
) (workspace.VTE, error) {
	return nil, errCapableAttach
}

func (*capableScheme) DeleteVTE(context.Context, workspace.VTEID) error {
	return errCapableDelete
}

func (s *capableScheme) OnDisconnect() <-chan struct{} { return s.disconnect }

func (*capableScheme) WaitConnected(context.Context) error { return errCapableConnect }

func (*capableScheme) InstallDataDir(context.Context) (string, error) {
	return "/capable", nil
}
