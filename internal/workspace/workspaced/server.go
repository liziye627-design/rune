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

// Package workspaced runs the workspace server of a data directory:
// the one process per machine that owns the terminals of every
// workspace on it, so they outlive every Rune client.
//
// It is what `rune --headless` runs, whether an operator started it or
// a Rune started it on demand, and the only way either gets one: the
// data directory's lock makes sure there is never a second.
package workspaced

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	grpc "google.golang.org/grpc"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace/remotescheme"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspacerpc"
	"unstable.build/rune/internal/workspace/workspaceserver"
)

// Options configures a workspace server.
type Options struct {
	// DataDir locates the server's lock and sockets.
	DataDir string
	// Scheme is the machine's root scheme. The server decorates it
	// with terminal ownership, serves every workspace under it, and
	// closes it on Close.
	Scheme schemeapi.Scheme
	// ServerOptions are appended to the RPC server's defaults.
	ServerOptions []grpc.ServerOption
	// RegisterServices, when set, installs extra services on the RPC
	// server before it starts serving. It is how the process hosting
	// the server exposes what else it owns on the same socket.
	RegisterServices func(*grpc.Server)
}

// Server is a running workspace server.
type Server struct {
	lock      *workspaceserver.Lock
	terminals *vtescheme.Server
	rpc       *workspacerpc.Server
	grpc      *grpc.Server
	vteLis    net.Listener
	cancel    context.CancelFunc
	serving   sync.WaitGroup
	done      chan struct{}

	mu       sync.Mutex
	serveErr error
	closing  bool

	closeOnce sync.Once
	closeErr  error
}

// Start takes the lock of opts.DataDir and serves on its sockets. It
// fails with [workspaceserver.ErrLocked] when another server holds the
// lock, and owns opts.Scheme either way: a server that did not start
// closes it.
func Start(opts Options) (*Server, error) {
	if opts.Scheme == nil {
		return nil, errors.New("workspaced: scheme is required")
	}
	if opts.DataDir == "" {
		_ = opts.Scheme.Close()
		return nil, errors.New("workspaced: data directory is required")
	}
	lock, err := workspaceserver.AcquireLock(opts.DataDir)
	if err != nil {
		_ = opts.Scheme.Close()
		return nil, err
	}
	s := &Server{
		lock:      lock,
		terminals: vtescheme.NewServer(opts.Scheme),
		done:      make(chan struct{}),
	}
	s.rpc = workspacerpc.NewServer(s.terminals, workspacerpc.CommandAuthorizerFunc(
		func(context.Context, workspaceapi.Cmd) error { return nil }))
	s.grpc = grpc.NewServer(append([]grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(remotescheme.ServerEnforcement),
	}, opts.ServerOptions...)...)
	workspacerpc.RegisterServices(s.grpc, s.rpc)
	if opts.RegisterServices != nil {
		opts.RegisterServices(s.grpc)
	}

	// Clients treat the RPC socket accepting as the server being
	// reachable, so the attachment socket is bound first: by the time
	// anyone can observe the server, both are there.
	s.vteLis, err = workspaceserver.Listen(workspaceserver.VTESocketPath(opts.DataDir))
	if err != nil {
		s.abort()
		return nil, err
	}
	lis, err := workspaceserver.Listen(workspaceserver.SocketPath(opts.DataDir))
	if err != nil {
		_ = s.vteLis.Close()
		s.abort()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.serving.Add(2)
	go debug.CapturePanicReport(func() {
		defer s.serving.Done()
		if err := s.terminals.Serve(ctx, s.vteLis); err != nil {
			s.fail(fmt.Errorf("serve terminal attachments: %w", err))
		}
	})
	go debug.CapturePanicReport(func() {
		defer s.serving.Done()
		if err := s.grpc.Serve(lis); err != nil {
			s.fail(fmt.Errorf("serve workspaces: %w", err))
		}
	})
	return s, nil
}

// abort tears down a server that never started serving.
func (s *Server) abort() {
	_ = s.rpc.Stop()
	_ = s.terminals.Close()
	_ = s.lock.Release()
}

// Scheme is the machine's root scheme as this server serves it: the
// one handed to Start, decorated with the terminals the server owns.
// Serving it elsewhere — to peers on the Rune network — gives them the
// same persistent terminals a client of the sockets gets. It is owned
// by the server and must not be closed by the caller.
func (s *Server) Scheme() vtescheme.Scheme {
	return s.terminals
}

// fail stops a server whose listener died on its own: a server that
// cannot be reached must not keep holding the lock, or nothing could
// replace it. A listener that dies because the server is closing is no
// reason to report.
func (s *Server) fail(err error) {
	s.mu.Lock()
	if s.serveErr == nil && !s.closing {
		s.serveErr = err
	}
	s.mu.Unlock()
	go debug.CapturePanicReport(func() { _ = s.Close() })
}

// Done is closed once the server has stopped, whether it was closed or
// its listeners failed.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// Err reports why the server stopped on its own, or nil when it was
// closed or is still serving.
func (s *Server) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serveErr
}

// Close stops serving, terminates every terminal and releases the lock.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.cancel()
		s.grpc.Stop()
		_ = s.vteLis.Close()
		s.serving.Wait()
		_ = s.rpc.Stop()
		s.closeErr = s.terminals.Close()
		if err := s.lock.Release(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
		close(s.done)
	})
	return s.closeErr
}
