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

package workspacerpc

import (
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi/workspacerpc"
	grpc "google.golang.org/grpc"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspacerpc/vterpc"
)

// RegisterServices installs every service a workspace server offers,
// including the repository-private terminal transport. Hosting them on
// one server keeps a client's files, commands and terminals on a single
// connection, so they share its authentication and lifetime.
//
// The terminal service is only offered when the scheme owns terminals;
// a client of a server without it sees the calls as unimplemented and
// maps them to [workspace.ErrVTEUnsupported].
func RegisterServices(grpcServer *grpc.Server, s *Server) {
	workspacerpc.RegisterSchemeServer(grpcServer, s)
	workspacerpc.RegisterFilesServer(grpcServer, s)
	workspacerpc.RegisterExecutorServer(grpcServer, s)
	workspacerpc.RegisterTerminalServer(grpcServer, s)
	if _, ok := s.s.(workspace.VTEOwner); ok {
		vterpc.RegisterVTEServer(grpcServer, NewVTEServer(s.s))
	}
}
