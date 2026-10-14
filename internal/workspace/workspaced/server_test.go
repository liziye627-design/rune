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
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacetest"
)

func TestStartRequiresItsOptions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		opts    Options
		wantMsg string
	}{
		{
			name:    "a data directory",
			opts:    Options{Scheme: newScheme(t, t.TempDir())},
			wantMsg: "data directory is required",
		},
		{
			name:    "a scheme",
			opts:    Options{DataDir: workspacetest.ShortTempDir(t)},
			wantMsg: "scheme is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Start(tc.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}

	t.Run("a server refused for its options still owns its scheme", func(t *testing.T) {
		t.Parallel()
		scheme := &closeRecorder{Scheme: newScheme(t, t.TempDir())}
		_, err := Start(Options{Scheme: scheme})
		require.Error(t, err)
		assert.True(t, scheme.closed.Load())
	})
}

// TestStartFailsWithoutLeaking pins that a server that does not start
// leaves nothing behind: not the scheme it was handed, and above all
// not the lock, which would keep every later server from starting.
func TestStartFailsWithoutLeaking(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		occupy  func(t *testing.T, dataDir string)
		wantIs  error
		wantMsg string
	}{
		{
			name: "another server holds the lock",
			occupy: func(t *testing.T, dataDir string) {
				start(t, dataDir)
			},
			wantIs:  workspaceserver.ErrLocked,
			wantMsg: "there's another instance of Rune holding the lock",
		},
		{
			name: "the attachment socket is taken",
			occupy: func(t *testing.T, dataDir string) {
				listen(t, workspaceserver.VTESocketPath(dataDir))
			},
			wantMsg: "already serving",
		},
		{
			name: "the workspace socket is taken",
			occupy: func(t *testing.T, dataDir string) {
				listen(t, workspaceserver.SocketPath(dataDir))
			},
			wantMsg: "already serving",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := workspacetest.ShortTempDir(t)
			tc.occupy(t, dataDir)
			scheme := &closeRecorder{Scheme: newScheme(t, t.TempDir())}

			_, err := Start(Options{DataDir: dataDir, Scheme: scheme})
			require.Error(t, err)
			if tc.wantIs != nil {
				assert.ErrorIs(t, err, tc.wantIs)
			}
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.True(t, scheme.closed.Load(),
				"a server that did not start still owns its scheme")
			if tc.wantIs == nil {
				lock, err := workspaceserver.AcquireLock(dataDir)
				require.NoError(t, err, "a failed start must release the lock")
				require.NoError(t, lock.Release())
			}
		})
	}
}

func TestServersOfDifferentDataDirsCoexist(t *testing.T) {
	t.Parallel()
	start(t, workspacetest.ShortTempDir(t))
	start(t, workspacetest.ShortTempDir(t))
}

// TestRegisterServicesSharesTheSocket pins that what the hosting
// process registers through Options.RegisterServices is reachable on
// the same socket as the workspace services, so a client needs one
// connection for both.
func TestRegisterServicesSharesTheSocket(t *testing.T) {
	t.Parallel()
	dataDir := workspacetest.ShortTempDir(t)
	s, err := Start(Options{
		DataDir: dataDir,
		Scheme:  newScheme(t, t.TempDir()),
		RegisterServices: func(grpcServer *grpc.Server) {
			healthpb.RegisterHealthServer(grpcServer, health.NewServer())
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	conn, err := workspaceserver.Dial(ctx, workspaceserver.SocketPath(dataDir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
}

// TestCloseReportsWhatItCouldNotRelease pins that closing a server gives
// up the lock whatever else fails to close, so a replacement can start,
// and reports the first thing it could not release.
func TestCloseReportsWhatItCouldNotRelease(t *testing.T) {
	t.Parallel()
	stuck := errors.New("the scheme would not close")

	for _, tc := range []struct {
		name        string
		schemeErr   error
		lockLetGo   bool
		wantIs      error
		wantNoError bool
	}{
		{
			name:        "nothing",
			wantNoError: true,
		},
		{
			name:      "its scheme",
			schemeErr: stuck,
			wantIs:    stuck,
		},
		{
			name:      "a lock it no longer held",
			lockLetGo: true,
			wantIs:    os.ErrClosed,
		},
		{
			name:      "its scheme before its lock",
			schemeErr: stuck,
			lockLetGo: true,
			wantIs:    stuck,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := workspacetest.ShortTempDir(t)
			scheme := &closeRecorder{Scheme: newScheme(t, t.TempDir()), err: tc.schemeErr}
			s, err := Start(Options{DataDir: dataDir, Scheme: scheme})
			require.NoError(t, err)
			if tc.lockLetGo {
				require.NoError(t, s.lock.Release())
			}

			err = s.Close()
			if tc.wantNoError {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantIs)
			}
			assert.Equal(t, err, s.Close(), "closing again reports the same")
			select {
			case <-s.Done():
			default:
				t.Fatal("a closed server is done")
			}
			assert.NoError(t, s.Err(), "a closed server did not stop on its own")
			lock, err := workspaceserver.AcquireLock(dataDir)
			require.NoError(t, err, "a closed server must not keep the lock")
			require.NoError(t, lock.Release())
		})
	}
}

// TestServerStopsWhenItsListenerFails pins that a server nobody can
// reach any more stops and gives up the lock, so a replacement can
// start, and says why it stopped.
func TestServerStopsWhenItsListenerFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("listener died")

	for _, tc := range []struct {
		name    string
		fail    func(s *Server)
		wantIs  error
		wantMsg string
	}{
		{
			name:    "its attachment listener dies",
			fail:    func(s *Server) { _ = s.vteLis.Close() },
			wantIs:  net.ErrClosed,
			wantMsg: "serve terminal attachments",
		},
		{
			name: "only its first failure is the reason",
			fail: func(s *Server) {
				s.fail(boom)
				s.fail(errors.New("a later failure"))
			},
			wantIs: boom,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := workspacetest.ShortTempDir(t)
			s, err := Start(Options{DataDir: dataDir, Scheme: newScheme(t, t.TempDir())})
			require.NoError(t, err)

			tc.fail(s)
			select {
			case <-s.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("a server that cannot be reached kept running")
			}
			require.ErrorIs(t, s.Err(), tc.wantIs)
			assert.Contains(t, s.Err().Error(), tc.wantMsg)

			lock, err := workspaceserver.AcquireLock(dataDir)
			require.NoError(t, err, "a stopped server must not keep the lock")
			require.NoError(t, lock.Release())
		})
	}
}

func newScheme(t *testing.T, root string) schemeapi.Scheme {
	t.Helper()
	uri, err := workspaceapi.CurrentUserHostURI(root)
	require.NoError(t, err)
	scheme, err := workspace.NewFileScheme(context.Background(), config.NopConfig(), uri)
	require.NoError(t, err)
	return scheme
}

func start(t *testing.T, dataDir string) *Server {
	t.Helper()
	return startAt(t, dataDir, t.TempDir())
}

func startAt(t *testing.T, dataDir, root string) *Server {
	t.Helper()
	s, err := Start(Options{DataDir: dataDir, Scheme: newScheme(t, root)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func listen(t *testing.T, path string) {
	t.Helper()
	lis, err := workspaceserver.Listen(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	go debug.CapturePanicReport(func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	})
}

func newClient(t *testing.T, root, dataDir string) vtescheme.Scheme {
	t.Helper()
	uri, err := workspaceapi.CurrentUserHostURI(root)
	require.NoError(t, err)
	client := vtescheme.NewClient(context.Background(), newScheme(t, root), uri,
		vtescheme.ClientOptions{DataDir: dataDir})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func vteCtx(ctx context.Context) context.Context {
	return vtescheme.WithClientVTEEnv(ctx, vtescheme.ClientVTEEnv{
		Publisher:     workspacetest.NopVTEHost{},
		Notifications: workspacetest.NopVTEHost{},
		Tabs:          workspacetest.NopVTEHost{},
		Config:        vte.DefaultConfig(),
	})
}

type closeRecorder struct {
	schemeapi.Scheme
	closed atomic.Bool
	// err, when set, is what Close reports once the scheme is closed.
	err error
}

func (s *closeRecorder) Close() error {
	s.closed.Store(true)
	if err := s.Scheme.Close(); err != nil {
		return err
	}
	return s.err
}
