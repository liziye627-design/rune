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

package meshrpc

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"unstable.build/rune/internal/runenet"
)

// Every runenet sentinel has an errors.Is branch somewhere in the
// editor: the prompt, the auto-join log level, the reconnect loop. All
// of them have to survive the hop through the daemon.
func TestErrorRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode codes.Code
	}{
		{"not started", runenet.ErrNotStarted, codes.FailedPrecondition},
		{"not authenticated", runenet.ErrNotAuthenticated, codes.Unauthenticated},
		{"subscription required", runenet.ErrSubscriptionRequired, codes.PermissionDenied},
		{"machine limit", runenet.ErrMachineLimit, codes.ResourceExhausted},
		{"peer not found", runenet.ErrPeerNotFound, codes.NotFound},
		{"machine removed", runenet.ErrMachineRemoved, codes.PermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("while joining: %w", tc.err)
			wire := StatusError(wrapped)
			st, ok := status.FromError(wire)
			require.True(t, ok)
			assert.Equal(t, tc.wantCode, st.Code())
			assert.Equal(t, wrapped.Error(), st.Message())

			got := ErrorFromStatus(wire)
			assert.True(t, errors.Is(got, tc.err), "got %v", got)
			assert.Equal(t, wrapped.Error(), got.Error())
		})
	}

	for _, tc := range []struct {
		err      error
		wantCode codes.Code
	}{
		{context.Canceled, codes.Canceled},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("while joining: %w", tc.err)
			wire := StatusError(wrapped)
			assert.Equal(t, tc.wantCode, status.Code(wire))

			got := ErrorFromStatus(wire)
			assert.True(t, errors.Is(got, tc.err), "got %v", got)
			assert.Equal(t, wrapped.Error(), got.Error())
		})
	}
}

func TestStatusErrorPassesThroughNonSentinels(t *testing.T) {
	t.Run("nil stays nil", func(t *testing.T) {
		assert.NoError(t, StatusError(nil))
		assert.NoError(t, ErrorFromStatus(nil))
	})

	t.Run("plain error travels as Unknown and comes back a status", func(t *testing.T) {
		wire := StatusError(errors.New("control server unreachable"))
		assert.Equal(t, codes.Unknown, status.Code(wire))
		got := ErrorFromStatus(wire)
		assert.Equal(t, codes.Unknown, status.Code(got))
		assert.Contains(t, got.Error(), "control server unreachable")
	})

	t.Run("an existing status keeps its code", func(t *testing.T) {
		wire := StatusError(status.Error(codes.Unavailable, "shutting down"))
		assert.Equal(t, codes.Unavailable, status.Code(wire))
	})

	t.Run("a transport error is returned as is", func(t *testing.T) {
		err := status.Error(codes.Unavailable, "connection refused")
		assert.Same(t, err, ErrorFromStatus(err))
	})
}

func TestStatusConversion(t *testing.T) {
	want := runenet.Status{
		Hostname:   "carbon",
		Addrs:      []netip.Addr{netip.MustParseAddr("100.64.0.1")},
		ControlURL: "https://control.example.com",
		State:      runenet.StateRunning,
		AuthURL:    "https://login.example.com/x",
		LoginName:  "me@example.com",
		LastError:  "boom",
		MachineID:  "42",
	}
	assert.Equal(t, want, statusFromProto(statusToProto(want)))
	assert.Equal(t, runenet.Status{}, statusFromProto(nil))

	t.Run("unparseable addresses are dropped", func(t *testing.T) {
		got := statusFromProto(&StatusResponse{Addrs: []string{"nope", "100.64.0.2"}})
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("100.64.0.2")}, got.Addrs)
	})
}

func TestPeersConversion(t *testing.T) {
	want := []runenet.Peer{
		{
			Hostname: "studio",
			DNSName:  "studio.rune.test",
			Addrs:    []netip.Addr{netip.MustParseAddr("100.64.0.3")},
			OS:       "macOS",
			Online:   true,
		},
		{Hostname: "builder"},
	}
	assert.Equal(t, want, peersFromProto(peersToProto(want)))
	assert.Empty(t, peersFromProto(nil))
}
