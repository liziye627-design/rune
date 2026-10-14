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

package workspaceserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"unstable.build/rune/internal/debug"
)

// startPollInterval is how often Connect retries the socket of a server
// it just started.
const startPollInterval = 20 * time.Millisecond

// DefaultStartTimeout bounds how long Connect waits for a freshly
// started workspace server to accept connections.
const DefaultStartTimeout = 30 * time.Second

// Config describes how to reach — and if necessary start — the
// workspace server owning a socket.
type Config struct {
	// Socket is the server's stable address; see [SocketPath].
	Socket string

	// Command builds the process that serves Socket. Connect only
	// invokes it when nothing is serving there yet.
	Command func(socket string) (*exec.Cmd, error)

	// StartTimeout defaults to [DefaultStartTimeout].
	StartTimeout time.Duration

	// DialOptions are appended to the transport defaults.
	DialOptions []grpc.DialOption
}

// Connect returns a connection to the workspace server serving
// cfg.Socket, starting one when none is.
//
// Concurrent callers cannot produce duplicate servers: a server takes
// the data directory's lock (see [AcquireLock]) before it binds the
// socket, so the loser of a start race exits and this call reaches the
// winner.
func Connect(ctx context.Context, cfg Config) (*grpc.ClientConn, error) {
	if cfg.Socket == "" {
		return nil, errors.New("workspaceserver: socket is required")
	}
	if conn, err := Dial(ctx, cfg.Socket, cfg.DialOptions...); err == nil {
		return conn, nil
	}
	if cfg.Command == nil {
		return nil, fmt.Errorf(
			"no workspace server serving %q and no command to start one",
			cfg.Socket)
	}
	if err := start(cfg); err != nil {
		return nil, err
	}
	return dialWhenServing(ctx, cfg)
}

// Dial connects to a workspace server already serving socket. It fails
// fast rather than queueing behind a server that may not exist.
func Dial(
	ctx context.Context, socket string, opts ...grpc.DialOption,
) (*grpc.ClientConn, error) {
	var probe net.Dialer
	conn, err := probe.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("dial %q: %w", socket, err)
	}
	_ = conn.Close()

	dialOpts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)
	// gRPC's unix resolver wants an absolute path as an authority-less
	// target, so the socket path follows an empty authority.
	client, err := grpc.NewClient("unix://"+socket, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("new client %q: %w", socket, err)
	}
	return client, nil
}

// start launches the server detached from the caller.
//
// Setsid rather than Setpgid: the server must survive the client's
// terminal, and a process merely in its own process group can still be
// stopped by job control when it touches the shared tty.
func start(cfg Config) error {
	cmd, err := cfg.Command(cfg.Socket)
	if err != nil {
		return fmt.Errorf("build workspace server command: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = new(syscall.SysProcAttr)
	}
	cmd.SysProcAttr.Setsid = true
	if cmd.Stdin == nil {
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			return fmt.Errorf("open %s: %w", os.DevNull, err)
		}
		defer devNull.Close()
		cmd.Stdin = devNull
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start workspace server: %w", err)
	}
	// Nobody here cares how the server exits, but a child that has
	// exited stays a zombie until its parent waits for it, and a server
	// that loses the start race exits at once.
	go debug.CapturePanicReport(func() { _ = cmd.Wait() })
	return nil
}

func dialWhenServing(
	ctx context.Context, cfg Config,
) (*grpc.ClientConn, error) {
	timeout := cfg.StartTimeout
	if timeout == 0 {
		timeout = DefaultStartTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(startPollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		conn, err := Dial(ctx, cfg.Socket, cfg.DialOptions...)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf(
				"wait for workspace server on %q: %w: %v",
				cfg.Socket, ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}
