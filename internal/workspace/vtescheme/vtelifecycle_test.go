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
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace/workspacerpc"
	"unstable.build/rune/internal/workspace/workspacerpc/vterpc"
)

// setupVTELifecycle serves one registry's terminal lifecycle over the
// workspace RPC surface and returns a client of it.
func setupVTELifecycle(
	t *testing.T, h *wireHarness,
) *workspacerpc.VTEClient {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	vterpc.RegisterVTEServer(srv, workspacerpc.NewVTEServer(h.scheme))
	go debug.CapturePanicReport(func() { _ = srv.Serve(lis) })
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return workspacerpc.NewVTEClient(conn, "")
}
