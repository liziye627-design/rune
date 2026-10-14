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

// Package vtescheme decorates a workspace scheme with terminal
// emulators that outlive the client that opened them.
//
// It is the only place in the repository that knows a terminal may
// live in another process. [NewServer] owns the emulators, [NewClient]
// and [NewConnClient] reach a server that does, and every other
// consumer simply uses [workspace.VTEOwner] without caring which side
// it holds.
package vtescheme

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/ernestrc/go-multierror"
	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	grpc "google.golang.org/grpc"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtewire"
	"unstable.build/rune/internal/workspace/workspacerpc"
	"unstable.build/rune/internal/workspace/workspaceserver"
)

// terminals is which terminals a workspace has and terminating one,
// which this package resolves to either the server owning them or a
// client of it. Opening an attachment is not part of it: that is a
// byte stream, not a request.
type terminals interface {
	ListVTEs(context.Context) ([]workspace.VTEInfo, error)
	DeleteVTE(context.Context, workspace.VTEID) error
}

// Scheme is a scheme that owns terminal emulators, whether directly or
// through a workspace server it is a client of.
type Scheme interface {
	schemeapi.Scheme
	workspace.VTEOwner
}

var (
	_ Scheme = (*Server)(nil)
	_ Scheme = (*clientScheme)(nil)
)

// NewServer decorates inner so it owns terminal emulators: their
// handlers, ptys, processes, rendered grids, scrollback and stable
// identities. Closing it terminates every terminal, so it belongs to a
// process whose lifetime is the machine's, not a client's.
//
// It hosts the terminals of every workspace under inner's root, each
// reached through [Server.Chroot].
func NewServer(inner schemeapi.Scheme) *Server {
	// A pty resize is driven from inside the emulator, so letting it
	// run inline would let one wedged terminal stall every other
	// terminal this server owns.
	resizes := workspace.NewAsyncTerminal(inner, headlessVTEHost{})
	s := newServer(inner, newSchemeVTEFactory(inner, resizes))
	s.resizes = resizes
	return s
}

func newServer(inner schemeapi.Scheme, factory VTEFactory) *Server {
	return &Server{
		Scheme:   inner,
		root:     filepath.Clean(inner.Root()),
		registry: NewVTERegistry(factory),
	}
}

// Server is a scheme whose terminals are canonical emulators it owns.
// Their lifecycle is served over the workspace's ordinary RPC surface;
// [Server.Serve] carries the attachments themselves.
type Server struct {
	schemeapi.Scheme
	// root is the workspace whose terminals this server reaches.
	root     string
	registry *VTERegistry
	resizes  *workspace.AsyncTerminal
	// view marks a server Chroot returned. It shares its terminals and
	// its scheme's resources with the server it came from, which is
	// the only one that may tear them down.
	view bool
}

// Chroot returns the server of the workspace rooted at path. Unlike the
// scheme it decorates, the result still owns terminals, scoped to that
// workspace: it is how one server keeps every workspace on its machine
// to its own terminals.
func (s *Server) Chroot(path string) (schemeapi.Scheme, error) {
	root, err := s.workspaceRoot(path)
	if err != nil {
		return nil, err
	}
	inner, err := s.Scheme.Chroot(root)
	if err != nil {
		return nil, err
	}
	return &Server{
		Scheme:   inner,
		root:     root,
		registry: s.registry,
		resizes:  s.resizes,
		view:     true,
	}, nil
}

// workspaceRoot resolves path the way the scheme resolves any path,
// relative to this server's root, so every client naming a workspace
// reaches the same terminals however it spells it.
func (s *Server) workspaceRoot(path string) (string, error) {
	if path == "" {
		return s.root, nil
	}
	uri, err := s.Scheme.URI(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %q: %w", path, err)
	}
	return filepath.Clean(uri.Path()), nil
}

// NewLoopback decorates inner so it owns terminal emulators in this
// process and reaches them the way a client of a workspace server
// does.
//
// It is the whole attachment protocol without a socket. A terminal
// hosted this way behaves exactly like one hosted elsewhere, which is
// what a process embedding the IDE needs when it has no separate
// workspace server to hand its terminals to.
func NewLoopback(inner schemeapi.Scheme) Scheme {
	server := NewServer(inner)
	s := &clientScheme{Scheme: server}
	s.workspaceRoot = func() (string, error) { return server.root, nil }
	s.open = func(string) (terminals, io.Closer, error) {
		return server, nil, nil
	}
	s.dial = func(ctx context.Context) (io.ReadWriteCloser, error) {
		client, host := net.Pipe()
		go debug.CapturePanicReport(func() {
			_ = server.Attach(ctx, host)
		})
		return client, nil
	}
	return s
}

// CreateVTE satisfies workspace.VTEOwner.
func (s *Server) CreateVTE(
	ctx context.Context, opts workspace.CreateVTEOptions,
) (workspace.VTE, error) {
	return s.registry.CreateVTE(ctx, s.root, opts)
}

// ListVTEs satisfies workspace.VTEOwner.
func (s *Server) ListVTEs(ctx context.Context) ([]workspace.VTEInfo, error) {
	return s.registry.ListVTEs(ctx, s.root)
}

// AttachVTE satisfies workspace.VTEOwner.
func (s *Server) AttachVTE(
	ctx context.Context, id workspace.VTEID,
) (workspace.VTE, error) {
	return s.registry.AttachVTE(ctx, s.root, id)
}

// DeleteVTE satisfies workspace.VTEOwner.
func (s *Server) DeleteVTE(ctx context.Context, id workspace.VTEID) error {
	return s.registry.DeleteVTE(ctx, s.root, id)
}

// Close terminates every terminal this server owns. A view owns
// nothing of its own: its scheme is a chroot sharing the resources of
// the server it came from, so closing it would close theirs.
func (s *Server) Close() error {
	if s.view {
		return nil
	}
	var ret error
	if err := s.registry.Close(); err != nil {
		ret = multierror.Append(ret, fmt.Errorf("close terminals: %w", err))
	}
	if s.resizes != nil {
		s.resizes.Close()
	}
	if err := s.Scheme.Close(); err != nil {
		ret = multierror.Append(ret, err)
	}
	return ret
}

// ClientOptions describes the workspace server that owns a machine's
// terminals and how to start it.
type ClientOptions struct {
	// DataDir locates the server; see [workspaceserver.SocketPath].
	DataDir string

	// Command is the program and arguments that start the server when
	// none is running. Only the process that knows it is rune can name
	// it: a host that guessed would hand its terminals to whatever
	// binary happens to be running. Without it a client only reaches a
	// server that is already running.
	Command []string
}

// NewClient decorates inner so the terminals of the workspace at uri
// belong to the persistent workspace server of this machine rather
// than to the calling process.
//
// Files and commands stay with inner: they are request/response and
// gain nothing from a hop. Terminals are long-lived state, so they live
// in a server that survives this process, and this scheme only hands
// out attachments to them. The server is started on the first terminal,
// so opening a workspace costs nothing extra.
func NewClient(
	ctx context.Context, inner schemeapi.Scheme,
	uri workspaceapi.URI, opts ClientOptions,
) Scheme {
	root := filepath.Clean(uri.Path())
	s := &clientScheme{Scheme: inner}
	s.workspaceRoot = func() (string, error) { return root, nil }
	s.open = func(root string) (terminals, io.Closer, error) {
		conn, err := connectServer(ctx, opts)
		if err != nil {
			return nil, nil, err
		}
		return workspacerpc.NewVTEClient(conn, root), conn, nil
	}
	s.dial = func(ctx context.Context) (io.ReadWriteCloser, error) {
		// The attachment socket only exists once the server is
		// serving, which is what the lifecycle connection guarantees.
		if _, err := s.terminals(ctx); err != nil {
			return nil, err
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix",
			workspaceserver.VTESocketPath(opts.DataDir))
	}
	return s
}

// DialAttachment opens a byte stream to the attachment socket of the
// workspace server of opts.DataDir, starting that server if none is.
//
// It is how a process that is not the client reaches those terminals:
// an ssh bridge attaches on behalf of a Rune running on another host.
// The client names its workspace in the attachment's handshake.
func DialAttachment(ctx context.Context, opts ClientOptions) (net.Conn, error) {
	// Connecting is what starts the server when none is running; the
	// attachment socket only exists once it is serving.
	conn, err := connectServer(ctx, opts)
	if err != nil {
		return nil, err
	}
	_ = conn.Close()

	var d net.Dialer
	return d.DialContext(ctx, "unix", workspaceserver.VTESocketPath(opts.DataDir))
}

func connectServer(
	ctx context.Context, opts ClientOptions,
) (*grpc.ClientConn, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf(
			"%w: no data directory to locate the workspace server",
			workspace.ErrVTEUnsupported)
	}
	cfg := workspaceserver.Config{
		Socket: workspaceserver.SocketPath(opts.DataDir),
	}
	if len(opts.Command) > 0 {
		cfg.Command = func(string) (*exec.Cmd, error) {
			return exec.Command(opts.Command[0], opts.Command[1:]...), nil
		}
	}
	return workspaceserver.Connect(ctx, cfg)
}

// NewConnClient decorates inner so its terminals belong to the
// workspace server already reachable through conn, which is how a
// remote workspace reaches the terminals of the host it is connected
// to.
//
// dial opens one attachment: a raw byte stream to that server's
// attachment socket, carried over whatever transport the caller
// already has. The workspace the terminals belong to is inner's root
// as the server resolves it, which is the only spelling of it every
// client of that server agrees on.
func NewConnClient(
	inner schemeapi.Scheme, conn grpc.ClientConnInterface,
	dial func(context.Context) (io.ReadWriteCloser, error),
) Scheme {
	s := &clientScheme{Scheme: inner, dial: dial}
	s.workspaceRoot = func() (string, error) {
		uri, err := inner.URI(".")
		if err != nil {
			return "", err
		}
		return filepath.Clean(uri.Path()), nil
	}
	s.open = func(root string) (terminals, io.Closer, error) {
		return workspacerpc.NewVTEClient(conn, root), nil, nil
	}
	return s
}

type clientScheme struct {
	schemeapi.Scheme
	// workspaceRoot names, to the server, the workspace this scheme's
	// terminals belong to.
	workspaceRoot func() (string, error)
	// open reaches the lifecycle of the terminals of root, returning
	// what must be closed with this scheme.
	open func(root string) (terminals, io.Closer, error)
	// dial opens the raw attachment transport.
	dial func(context.Context) (io.ReadWriteCloser, error)

	mu      sync.Mutex
	root    string
	hasRoot bool
	host    terminals
	closer  io.Closer
	// opening is closed once the connection being opened settles. It
	// is opened without mu held: it may start the server and wait for
	// it to serve, and nothing else may wait behind that.
	opening chan struct{}
	// closes counts Close calls, so a connection opened across one is
	// let go of rather than kept by a scheme that already let go.
	closes int
}

func (s *clientScheme) terminals(ctx context.Context) (terminals, error) {
	for {
		s.mu.Lock()
		if s.host != nil {
			host := s.host
			s.mu.Unlock()
			return host, nil
		}
		root, err := s.rootLocked()
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		wait := s.opening
		if wait == nil {
			s.opening = make(chan struct{})
		}
		closes := s.closes
		s.mu.Unlock()
		if wait == nil {
			return s.openOnce(root, closes)
		}
		// The connection being opened may fail for its own caller's
		// reasons, so this caller looks again.
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// openOnce opens the connection terminals found missing, and owns
// s.opening until it settles.
func (s *clientScheme) openOnce(root string, closes int) (terminals, error) {
	host, closer, err := s.open(root)
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.opening)
	s.opening = nil
	if err != nil {
		return nil, err
	}
	if s.closes != closes {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, errors.New("vtescheme: closed while reaching the workspace server")
	}
	s.host, s.closer = host, closer
	return host, nil
}

func (s *clientScheme) workspace() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rootLocked()
}

func (s *clientScheme) rootLocked() (string, error) {
	if !s.hasRoot {
		root, err := s.workspaceRoot()
		if err != nil {
			return "", fmt.Errorf("resolve workspace root: %w", err)
		}
		s.root, s.hasRoot = root, true
	}
	return s.root, nil
}

// Close releases this client's connection to the workspace server. The
// server keeps running: its terminals outlive the process that opened
// them.
func (s *clientScheme) Close() error {
	s.mu.Lock()
	closer := s.closer
	s.closer, s.host = nil, nil
	s.closes++
	s.mu.Unlock()
	var ret error
	if closer != nil {
		if err := closer.Close(); err != nil {
			ret = multierror.Append(ret, fmt.Errorf("close workspace server connection: %w", err))
		}
	}
	if err := s.Scheme.Close(); err != nil {
		ret = multierror.Append(ret, err)
	}
	return ret
}

// CreateVTE satisfies workspace.VTEOwner.
func (s *clientScheme) CreateVTE(
	ctx context.Context, opts workspace.CreateVTEOptions,
) (workspace.VTE, error) {
	return s.attach(ctx, &vtewire.Hello{
		Create: vtewire.CreateOptions{
			CommandAndArgs: opts.CommandAndArgs,
			MaxLines:       opts.MaxLines,
		},
		Width:  opts.Width,
		Height: opts.Height,
	})
}

// ListVTEs satisfies workspace.VTEOwner.
func (s *clientScheme) ListVTEs(ctx context.Context) ([]workspace.VTEInfo, error) {
	host, err := s.terminals(ctx)
	if err != nil {
		return nil, err
	}
	return host.ListVTEs(ctx)
}

// AttachVTE satisfies workspace.VTEOwner.
func (s *clientScheme) AttachVTE(
	ctx context.Context, id workspace.VTEID,
) (workspace.VTE, error) {
	return s.attach(ctx, &vtewire.Hello{VTEID: string(id)})
}

// attach opens one attachment and builds the local emulator that
// replays it. The browser collaborators travel with ctx because they
// belong to the window the terminal is opened in, not to the scheme:
// one scheme serves every window of a workspace.
func (s *clientScheme) attach(
	ctx context.Context, hello *vtewire.Hello,
) (workspace.VTE, error) {
	env, err := ClientVTEEnvFrom(ctx)
	if err != nil {
		return nil, err
	}
	if hello.Root, err = s.workspace(); err != nil {
		return nil, err
	}
	if hello.Width <= 0 {
		hello.Width = env.Config.WidthHint
	}
	if hello.Height <= 0 {
		hello.Height = env.Config.HeightHint
	}
	rw, err := s.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial terminal attachment: %w", err)
	}
	return NewClientVTE(vtewire.NewConn(rw), hello, env)
}

// DeleteVTE satisfies workspace.VTEOwner.
func (s *clientScheme) DeleteVTE(ctx context.Context, id workspace.VTEID) error {
	host, err := s.terminals(ctx)
	if err != nil {
		return err
	}
	return host.DeleteVTE(ctx, id)
}
