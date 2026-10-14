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

package workspacessh

import (
	"errors"
	"fmt"
	"net"
	"os"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace/remotescheme"
	tworkspacerpc "unstable.build/rune/internal/workspace/workspacerpc"
)

// NewSchemeServer builds the gRPC server used to serve workspacerpc
// over an SSH-tunneled stdio pipe. It always installs
// [remotescheme.ServerEnforcement] so the client's keepalive pings are
// not punished with GOAWAY too_many_pings.
func NewSchemeServer(opts ...grpc.ServerOption) *grpc.Server {
	return grpc.NewServer(append([]grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(remotescheme.ServerEnforcement),
	}, opts...)...)
}

// StartSchemeServer installs server to handle incoming workspacerpc requests
// over the calling process' os.Stdin and sends responses over os.Stdout.
func StartSchemeServer(
	logger *log.Logger, server *tworkspacerpc.Server,
	grpcServer *grpc.Server,
) error {
	lis := newStdioListener(
		logger, nil /*reader*/, nil, /*writer*/
		true, /* use stdio instead of reader and writer */
		func() {
			logger.Debugf("connection closed unexpectedly")
			go debug.CapturePanicReport(func() {
				grpcServer.Stop()
			})
		})
	tworkspacerpc.RegisterServices(grpcServer, server)
	if line, err := encodeServerReady(); err == nil {
		_, _ = os.Stderr.WriteString(line)
	}
	if err := grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

type stdioListener struct {
	writer   *os.File
	reader   *os.File
	acceptCh chan struct{}
	onlyConn net.Conn
	onClose  func()
	stdio    bool
	addr     net.Addr
	logger   *log.Logger
}

func newStdioListener(
	logger *log.Logger,
	reader, writer *os.File,
	stdio bool,
	onClose func(),
) *stdioListener {
	ret := new(stdioListener)
	ret.reader = reader
	ret.logger = logger
	ret.writer = writer
	ret.stdio = stdio
	ret.addr = newStdioAddr("localaddr")
	ret.acceptCh = make(chan struct{}, 1)
	ret.acceptCh <- struct{}{}
	ret.onClose = onClose
	return ret
}

func (lis *stdioListener) Accept() (net.Conn, error) {
	_, ok := <-lis.acceptCh
	if !ok {
		return nil, errors.New("closed listener")
	}

	conn, err := newStdConn(lis.logger, lis.reader, lis.writer,
		lis.stdio, lis.onClose)
	if err != nil {
		return nil, err
	}
	lis.onlyConn = conn

	return lis.onlyConn, nil
}

func (lis *stdioListener) Close() error {
	close(lis.acceptCh)
	return nil
}

func (lis *stdioListener) Addr() net.Addr {
	return lis.addr
}

type stdioAddr struct {
	s string
}

func newStdioAddr(s string) *stdioAddr {
	return &stdioAddr{s}
}

func (a *stdioAddr) Network() string {
	return "stdio"
}

func (a *stdioAddr) String() string {
	return a.s
}
