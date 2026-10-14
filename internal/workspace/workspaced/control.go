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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"unstable.build/rune/internal/debug"
)

// Control is what the server lets a process without an RPC client do
// to it over [workspaceserver.ControlSocketPath]: one command line per
// connection, answered with "ok" or "err <message>".
//
// The commands are "network up", "network down" and "stop". A command
// whose hook is nil is answered with an error.
type Control struct {
	// NetworkUp joins the Rune network; NetworkDown leaves it.
	NetworkUp   func(context.Context) error
	NetworkDown func(context.Context) error
	// Stop asks the hosting process to stop the server. It returns
	// once the stop is under way, not once the server stopped.
	Stop func()
}

const (
	controlReplyOK  = "ok"
	controlReplyErr = "err"
)

// controlReadTimeout bounds how long a client may take to send its
// command line.
const controlReadTimeout = 10 * time.Second

// controlTimeout bounds a command: joining the network can wait on a
// login nobody completes.
const controlTimeout = time.Minute

// controlMaxLine bounds a command line; no command is longer.
const controlMaxLine = 256

// serveControl answers commands on lis until lis is closed. ctx bounds
// the commands but does not unblock Accept.
func serveControl(ctx context.Context, lis net.Listener, ctl Control) error {
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go debug.CapturePanicReport(func() {
			defer func() { _ = conn.Close() }()
			_ = conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
			raw, err := bufio.NewReaderSize(conn, controlMaxLine).ReadSlice('\n')
			line := string(raw)
			if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
				if errors.Is(err, bufio.ErrBufferFull) {
					err = errors.New("command too long")
				}
				_ = writeControlReply(conn, fmt.Errorf("read command: %w", err))
				return
			}
			_ = conn.SetReadDeadline(time.Time{})
			cmdCtx, cancel := context.WithTimeout(ctx, controlTimeout)
			defer cancel()
			err = ctl.run(cmdCtx, strings.TrimSpace(line))
			if err != nil {
				log.Debugf("workspaced: control %q: %v", strings.TrimSpace(line), err)
			}
			_ = writeControlReply(conn, err)
		})
	}
}

func (c Control) run(ctx context.Context, cmd string) error {
	switch cmd {
	case "network up":
		if c.NetworkUp == nil {
			return errors.New("this server hosts no network")
		}
		return c.NetworkUp(ctx)
	case "network down":
		if c.NetworkDown == nil {
			return errors.New("this server hosts no network")
		}
		return c.NetworkDown(ctx)
	case "stop":
		if c.Stop == nil {
			return errors.New("this server cannot be stopped from here")
		}
		c.Stop()
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// writeControlReply sends one answer line. An error message is
// flattened to a single line so the framing survives whatever the
// command reported.
func writeControlReply(w io.Writer, err error) error {
	line := controlReplyOK + "\n"
	if err != nil {
		msg := strings.Join(strings.Fields(err.Error()), " ")
		line = controlReplyErr + " " + msg + "\n"
	}
	_, werr := io.WriteString(w, line)
	return werr
}
