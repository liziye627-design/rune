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

// Package workspaceserver locates, locks and reaches the workspace
// server of a data directory: the one process per machine and data
// directory whose lifetime is independent of any Rune client.
//
// The server owns canonical terminal emulators for every workspace of
// the machine, so tying it to the connection that started it would
// terminate every terminal when a Rune client exits or an ssh
// transport drops. It therefore listens on stable sockets under the
// data directory that successive and simultaneous clients dial, and
// holds a lock that keeps a second server from starting beside it.
package workspaceserver

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dirPerm keeps the socket directory private to its owner: anyone able
// to connect can run commands in the workspace.
const dirPerm os.FileMode = 0o700

// socketPerm restricts the socket itself. Linux enforces socket
// permissions on connect; macOS does not, which is why the containing
// directory is the primary control.
const socketPerm os.FileMode = 0o600

// SocketDir is the directory holding every workspace server socket
// under dataDir.
func SocketDir(dataDir string) string {
	return filepath.Join(dataDir, "servers")
}

// SocketPath returns the stable socket of the workspace server of
// dataDir.
func SocketPath(dataDir string) string {
	return filepath.Join(SocketDir(dataDir), "rune.sock")
}

// VTESocketPath returns the socket carrying terminal attachments to the
// workspace server of dataDir.
//
// Attachments get their own socket because they are a raw byte stream
// between a client's terminal emulator and a pty, not a request and a
// response. Keeping them off the RPC socket leaves that one free to
// answer while a terminal is producing output at full rate.
func VTESocketPath(dataDir string) string {
	return filepath.Join(SocketDir(dataDir), "rune.vte.sock")
}

// Listen binds the workspace server socket at path.
//
// A socket left behind by a server that died without unlinking it is
// removed only after a dial proves nobody is serving on it. The dial
// and the bind are not atomic, so it is the data directory's [Lock],
// which the caller must hold, that keeps a live server from being
// displaced by a late starter.
//
// Accept rides out the failures accept(2) reports while a later accept
// can still succeed, such as the process running out of file
// descriptors, so they never end an accept loop.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", path, err)
	}
	if err := os.Chmod(path, socketPerm); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return newPatientListener(lis), nil
}

// maxAcceptBackoff caps the pause between accepts that keep failing
// transiently.
const maxAcceptBackoff = time.Second

// patientListener backs off and accepts again on a transient accept
// failure, the way net/http and gRPC do: the server owns every terminal
// of the machine, and a loop that ended on such a failure would end
// them all with it.
type patientListener struct {
	net.Listener
	closeOnce sync.Once
	closed    chan struct{}
}

func newPatientListener(lis net.Listener) net.Listener {
	return &patientListener{Listener: lis, closed: make(chan struct{})}
}

func (l *patientListener) Accept() (net.Conn, error) {
	var delay time.Duration
	for {
		conn, err := l.Listener.Accept()
		var temp interface{ Temporary() bool }
		if err == nil || !errors.As(err, &temp) || !temp.Temporary() {
			return conn, err
		}
		delay = min(max(2*delay, 5*time.Millisecond), maxAcceptBackoff)
		select {
		case <-time.After(delay):
		case <-l.closed:
		}
	}
}

func (l *patientListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

func removeStaleSocket(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat socket: %w", err)
	}
	conn, err := net.Dial("unix", path)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("workspace server already serving on %q", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}
