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
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
	"unstable.build/rune/internal/workspace/workspacerpc"
	"unstable.build/rune/internal/workspace/workspacerpc/vterpc"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacetest"
)

func TestServerWorkspaceRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	s := newTestServerScheme(t, base)

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "", want: base},
		{path: ".", want: base},
		{path: "alpha", want: filepath.Join(base, "alpha")},
		{path: "./alpha/", want: filepath.Join(base, "alpha")},
		{path: filepath.Join(base, "alpha", "..", "beta"), want: filepath.Join(base, "beta")},
		{path: "/somewhere/else/", want: "/somewhere/else"},
		{path: "~", want: home},
		{path: "~/src", want: filepath.Join(home, "src")},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			got, err := s.workspaceRoot(tc.path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClientSchemeWithoutServer(t *testing.T) {
	t.Parallel()
	vteCtx := WithClientVTEEnv(context.Background(), testClientVTEEnv())

	for _, tc := range []struct {
		name    string
		opts    ClientOptions
		wantIs  error
		wantMsg string
	}{
		{
			name:   "no data directory to find a server in",
			opts:   ClientOptions{},
			wantIs: workspace.ErrVTEUnsupported,
		},
		{
			name:    "no server running and no command to start one",
			opts:    ClientOptions{DataDir: workspacetest.ShortTempDir(t)},
			wantMsg: "no command to start one",
		},
		{
			name: "a command that cannot start a server",
			opts: ClientOptions{
				DataDir: workspacetest.ShortTempDir(t),
				Command: []string{"/nonexistent/rune", "--headless"},
			},
			wantMsg: "start workspace server",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			uri, err := workspaceapi.CurrentUserHostURI(dir)
			require.NoError(t, err)
			client := NewClient(context.Background(), fileScheme(t, dir), uri, tc.opts)
			t.Cleanup(func() { _ = client.Close() })

			for name, call := range map[string]func() error{
				"list": func() error {
					_, err := client.ListVTEs(context.Background())
					return err
				},
				"delete": func() error {
					return client.DeleteVTE(context.Background(), workspace.NewVTEID())
				},
				"create": func() error {
					_, err := client.CreateVTE(vteCtx, workspace.CreateVTEOptions{})
					return err
				},
				"attach": func() error {
					_, err := client.AttachVTE(vteCtx, workspace.NewVTEID())
					return err
				},
			} {
				err := call()
				require.Error(t, err, name)
				if tc.wantIs != nil {
					assert.ErrorIs(t, err, tc.wantIs, name)
				}
				assert.Contains(t, err.Error(), tc.wantMsg, name)
			}
		})
	}
}

type nopTerminals struct{}

func (nopTerminals) ListVTEs(context.Context) ([]workspace.VTEInfo, error) {
	return nil, nil
}

func (nopTerminals) DeleteVTE(context.Context, workspace.VTEID) error {
	return nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// Reaching the workspace server may mean starting it and waiting for
// it to serve. Closing the scheme, or giving up on a call, must not
// wait behind that, and a connection made after Close is let go of.
func TestClientSchemeReachingTheServerHoldsNothingUp(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	released := make(chan struct{})
	s := &clientScheme{
		Scheme:        fileScheme(t, t.TempDir()),
		workspaceRoot: func() (string, error) { return "/w", nil },
		open: func(string) (terminals, io.Closer, error) {
			close(entered)
			<-release
			return nopTerminals{}, closerFunc(func() error {
				close(released)
				return nil
			}), nil
		},
	}
	within := func(done <-chan struct{}, msg string) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal(msg)
		}
	}

	listed := make(chan error, 1)
	go debug.CapturePanicReport(func() {
		_, err := s.ListVTEs(context.Background())
		listed <- err
	})
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	deleted := make(chan struct{})
	go debug.CapturePanicReport(func() {
		defer close(deleted)
		assert.ErrorIs(t, s.DeleteVTE(ctx, workspace.NewVTEID()), context.Canceled)
	})
	cancel()
	within(deleted, "a call waiting on the server ignored its context")

	closed := make(chan struct{})
	go debug.CapturePanicReport(func() {
		_ = s.Close()
		close(closed)
	})
	within(closed, "Close waited on the server")

	releaseOnce.Do(func() { close(release) })
	within(released, "a connection made after Close was kept")
	assert.Error(t, <-listed)
}

func TestClientSchemeRequiresATerminalHost(t *testing.T) {
	t.Parallel()
	uri, err := workspaceapi.CurrentUserHostURI(t.TempDir())
	require.NoError(t, err)
	client := NewClient(context.Background(), nil, uri, ClientOptions{})

	for _, ctx := range []context.Context{
		context.Background(),
		WithClientVTEEnv(context.Background(), ClientVTEEnv{}),
	} {
		_, err := client.CreateVTE(ctx, workspace.CreateVTEOptions{})
		assert.ErrorIs(t, err, workspace.ErrVTEUnsupported)
	}
}

// TestConnClientNamesItsWorkspace pins that a client reaching a server
// over someone else's transport names its workspace the way that
// server resolves it: the only spelling every client of the server
// agrees on.
func TestConnClientNamesItsWorkspace(t *testing.T) {
	t.Parallel()
	vteCtx := WithClientVTEEnv(context.Background(), testClientVTEEnv())

	for _, tc := range []struct {
		name     string
		resolve  func(string) (workspaceapi.URI, error)
		wantRoot string
		wantErr  string
	}{
		{
			name: "the server's spelling of the root, cleaned",
			resolve: func(string) (workspaceapi.URI, error) {
				return workspaceapi.ParseURI("file:///srv/ws/./")
			},
			wantRoot: "/srv/ws",
		},
		{
			name: "a server that cannot say fails the call before dialing",
			resolve: func(string) (workspaceapi.URI, error) {
				return workspaceapi.URI{}, errors.New("transport down")
			},
			wantErr: "resolve workspace root: transport down",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lifecycle := newRecordingVTEService(t)
			inner := &resolvingScheme{NopScheme: &workspacetest.NopScheme{}, resolve: tc.resolve}
			var hellos []*vtewire.Hello
			var mu sync.Mutex
			client := NewConnClient(inner, lifecycle.conn,
				func(context.Context) (io.ReadWriteCloser, error) {
					local, remote := net.Pipe()
					go debug.CapturePanicReport(func() {
						conn := vtewire.NewConn(remote)
						hello, err := vtewire.Accept(conn)
						if err == nil {
							mu.Lock()
							hellos = append(hellos, hello)
							mu.Unlock()
						}
						_ = conn.WriteFrame(&vtewire.HelloErr{Code: vtewire.CodeNotFound})
						_ = remote.Close()
					})
					return local, nil
				})

			_, attachErr := client.AttachVTE(vteCtx, workspace.NewVTEID())
			_, listErr := client.ListVTEs(context.Background())
			deleteErr := client.DeleteVTE(context.Background(), workspace.NewVTEID())

			if tc.wantErr != "" {
				for _, err := range []error{attachErr, listErr, deleteErr} {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.wantErr)
				}
				assert.Empty(t, hellos, "nothing may be dialed for an unknown workspace")
				assert.Empty(t, lifecycle.roots())
				return
			}
			require.ErrorIs(t, attachErr, workspace.ErrVTENotFound)
			require.NoError(t, listErr)
			require.NoError(t, deleteErr)
			require.Len(t, hellos, 1)
			assert.Equal(t, tc.wantRoot, hellos[0].Root)
			assert.Equal(t, []string{tc.wantRoot, tc.wantRoot}, lifecycle.roots())
			assert.Equal(t, 1, inner.calls(),
				"the root is resolved once and reused")
		})
	}
}

func fileScheme(t *testing.T, root string) schemeapi.Scheme {
	t.Helper()
	uri, err := workspaceapi.CurrentUserHostURI(root)
	require.NoError(t, err)
	scheme, err := workspace.NewFileScheme(context.Background(), config.NopConfig(), uri)
	require.NoError(t, err)
	return scheme
}

func newTestServerScheme(t *testing.T, root string) *Server {
	t.Helper()
	server := NewServer(fileScheme(t, root))
	t.Cleanup(func() { _ = server.Close() })
	return server
}

// newTestClient is a workspace's client of the server serving dataDir.
func newTestClient(t *testing.T, root, dataDir string) Scheme {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	uri, err := workspaceapi.CurrentUserHostURI(root)
	require.NoError(t, err)
	client := NewClient(context.Background(), fileScheme(t, root), uri,
		ClientOptions{DataDir: dataDir})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// serveTerminals serves server the way a workspace server does: its
// lifecycle on the workspace socket of dataDir and its attachments on
// the socket beside it.
func serveTerminals(t *testing.T, server *Server, dataDir string) {
	t.Helper()
	lis, err := workspaceserver.Listen(workspaceserver.SocketPath(dataDir))
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	vterpc.RegisterVTEServer(grpcServer, workspacerpc.NewVTEServer(server))
	go debug.CapturePanicReport(func() { _ = grpcServer.Serve(lis) })
	t.Cleanup(grpcServer.Stop)

	vteLis, err := workspaceserver.Listen(workspaceserver.VTESocketPath(dataDir))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go debug.CapturePanicReport(func() {
		defer close(served)
		_ = server.Serve(ctx, vteLis)
	})
	t.Cleanup(func() {
		cancel()
		_ = vteLis.Close()
		<-served
	})
}

func chrootOwner(t *testing.T, s schemeapi.Scheme, path string) workspace.VTEOwner {
	t.Helper()
	view, err := s.Chroot(path)
	require.NoError(t, err)
	owner, ok := view.(workspace.VTEOwner)
	require.True(t, ok, "a view of a server must keep owning terminals")
	return owner
}

func createCat(t *testing.T, owner workspace.VTEOwner) workspace.VTE {
	t.Helper()
	v, err := owner.CreateVTE(context.Background(), workspace.CreateVTEOptions{
		CommandAndArgs: []string{"cat"},
		Width:          20, Height: 10,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func assertListed(t *testing.T, owner workspace.VTEOwner, want ...workspace.VTEID) {
	t.Helper()
	infos, err := owner.ListVTEs(context.Background())
	require.NoError(t, err)
	got := make([]workspace.VTEID, 0, len(infos))
	for _, info := range infos {
		got = append(got, info.ID)
	}
	assert.ElementsMatch(t, want, got)
}

// testClientVTEEnv is the browser a test does not have. The terminal
// still runs a real emulator; only what it would notify is inert.
func testClientVTEEnv() ClientVTEEnv {
	return ClientVTEEnv{
		Publisher:     workspacetest.NopVTEHost{},
		Notifications: workspacetest.NopVTEHost{},
		Tabs:          workspacetest.NopVTEHost{},
		Config:        vte.DefaultConfig(),
	}
}

// resolvingScheme answers URI the way a remote server would, counting
// how often it is asked.
type resolvingScheme struct {
	*workspacetest.NopScheme
	resolve func(string) (workspaceapi.URI, error)

	mu sync.Mutex
	n  int
}

func (s *resolvingScheme) URI(path string) (workspaceapi.URI, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return s.resolve(path)
}

func (s *resolvingScheme) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// recordingVTEService records which workspace each lifecycle request
// named.
type recordingVTEService struct {
	vterpc.UnimplementedVTEServer
	conn *grpc.ClientConn

	mu   sync.Mutex
	seen []string
}

func newRecordingVTEService(t *testing.T) *recordingVTEService {
	t.Helper()
	s := new(recordingVTEService)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	vterpc.RegisterVTEServer(srv, s)
	go debug.CapturePanicReport(func() { _ = srv.Serve(lis) })
	t.Cleanup(srv.Stop)
	s.conn, err = grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.conn.Close() })
	return s
}

func (s *recordingVTEService) List(
	_ context.Context, req *vterpc.ListRequest,
) (*vterpc.ListResponse, error) {
	s.record(req.GetRoot())
	return new(vterpc.ListResponse), nil
}

func (s *recordingVTEService) Delete(
	_ context.Context, req *vterpc.DeleteRequest,
) (*vterpc.DeleteResponse, error) {
	s.record(req.GetRoot())
	return new(vterpc.DeleteResponse), nil
}

func (s *recordingVTEService) record(root string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, root)
}

func (s *recordingVTEService) roots() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}
