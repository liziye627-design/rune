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
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// TestControlSocket pins the one-line protocol a process without an
// RPC client drives the server with, and that every command is
// answered, so a client never waits on a server that has nothing to
// say.
func TestControlSocket(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		control   func(calls *atomic.Int32) Control
		send      string
		want      string
		wantCalls int32
	}{
		{
			name: "network up",
			control: func(calls *atomic.Int32) Control {
				return Control{NetworkUp: func(context.Context) error {
					calls.Add(1)
					return nil
				}}
			},
			send:      "network up\n",
			want:      "ok",
			wantCalls: 1,
		},
		{
			name: "network down reports what failed on one line",
			control: func(calls *atomic.Int32) Control {
				return Control{NetworkDown: func(context.Context) error {
					calls.Add(1)
					return errors.New("node is\nnot running")
				}}
			},
			send:      "network down\n",
			want:      "err node is not running",
			wantCalls: 1,
		},
		{
			name: "stop",
			control: func(calls *atomic.Int32) Control {
				return Control{Stop: func() { calls.Add(1) }}
			},
			send:      "stop\n",
			want:      "ok",
			wantCalls: 1,
		},
		{
			name: "a command without its newline",
			control: func(calls *atomic.Int32) Control {
				return Control{Stop: func() { calls.Add(1) }}
			},
			send:      "stop",
			want:      "ok",
			wantCalls: 1,
		},
		{
			name:    "network up on a server without a network",
			control: func(*atomic.Int32) Control { return Control{} },
			send:    "network up\n",
			want:    "err this server hosts no network",
		},
		{
			name:    "stop on a server nobody can stop",
			control: func(*atomic.Int32) Control { return Control{} },
			send:    "stop\n",
			want:    "err this server cannot be stopped from here",
		},
		{
			name:    "an unknown command",
			control: func(*atomic.Int32) Control { return Control{} },
			send:    "reboot\n",
			want:    `err unknown command "reboot"`,
		},
		{
			name:    "nothing",
			control: func(*atomic.Int32) Control { return Control{} },
			send:    "",
			want:    "err read command: EOF",
		},
		{
			name:    "a line too long to be a command",
			control: func(*atomic.Int32) Control { return Control{} },
			send:    strings.Repeat("x", controlMaxLine+1) + "\n",
			want:    "err read command: command too long",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			dataDir := workspacetest.ShortTempDir(t)
			s, err := Start(Options{
				DataDir: dataDir,
				Scheme:  newScheme(t, t.TempDir()),
				Control: tc.control(&calls),
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })

			conn, err := net.DialTimeout("unix",
				workspaceserver.ControlSocketPath(dataDir), 5*time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			_, err = conn.Write([]byte(tc.send))
			require.NoError(t, err)
			require.NoError(t, conn.(*net.UnixConn).CloseWrite())

			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			reply, err := bufio.NewReader(conn).ReadString('\n')
			require.NoError(t, err)
			assert.Equal(t, tc.want, strings.TrimSuffix(reply, "\n"))
			assert.Equal(t, tc.wantCalls, calls.Load())
		})
	}
}
