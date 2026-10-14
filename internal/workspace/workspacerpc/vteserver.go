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
	"context"
	"errors"
	"path/filepath"

	"github.com/unstablebuild/rune-go-sdk/api/schemeapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspacerpc/vterpc"
)

// VTEServer exposes the lifecycle of the terminals a workspace owns over
// the repository-private VTE service.
//
// Attachments are deliberately absent: they are a raw byte stream
// between a client's emulator and a pty, served on a socket of their
// own (see workspace/vtescheme and workspace/vtewire).
type VTEServer struct {
	vterpc.UnimplementedVTEServer

	s schemeapi.Scheme
}

var _ vterpc.VTEServer = (*VTEServer)(nil)

// NewVTEServer allocates storage for a new VTEServer and initializes
// it with the scheme whose terminals to serve. Requests naming a
// workspace are served by the scheme chrooted to it, which must keep
// owning terminals: that is how one server scopes the terminals of
// every workspace it hosts.
func NewVTEServer(s schemeapi.Scheme) *VTEServer {
	if s == nil {
		panic("workspacerpc: scheme is required")
	}
	return &VTEServer{s: s}
}

// owner resolves the terminals of the workspace rooted at root.
// Chrooting a scheme to its own root is the identity, and skipping it
// keeps a scheme that only reaches terminals elsewhere, rather than
// owning them, serving its own workspace.
func (s *VTEServer) owner(root string) (workspace.VTEOwner, error) {
	scheme := s.s
	if root != "" && filepath.Clean(root) != filepath.Clean(s.s.Root()) {
		var err error
		if scheme, err = s.s.Chroot(root); err != nil {
			return nil, err
		}
	}
	owner, ok := scheme.(workspace.VTEOwner)
	if !ok {
		return nil, workspace.ErrVTEUnsupported
	}
	return owner, nil
}

// List satisfies vterpc.VTEServer.
func (s *VTEServer) List(
	ctx context.Context, req *vterpc.ListRequest,
) (*vterpc.ListResponse, error) {
	owner, err := s.owner(req.GetRoot())
	if err != nil {
		return nil, vteStatus(err)
	}
	infos, err := owner.ListVTEs(ctx)
	if err != nil {
		return nil, vteStatus(err)
	}
	resp := &vterpc.ListResponse{Vtes: make([]*vterpc.Info, 0, len(infos))}
	for _, info := range infos {
		msg := new(vterpc.Info)
		msg.FromModel(info)
		resp.Vtes = append(resp.Vtes, msg)
	}
	return resp, nil
}

// Delete satisfies vterpc.VTEServer.
func (s *VTEServer) Delete(
	ctx context.Context, req *vterpc.DeleteRequest,
) (*vterpc.DeleteResponse, error) {
	owner, err := s.owner(req.GetRoot())
	if err != nil {
		return nil, vteStatus(err)
	}
	if err := owner.DeleteVTE(ctx, workspace.VTEID(req.GetId())); err != nil {
		return nil, vteStatus(err)
	}
	return new(vterpc.DeleteResponse), nil
}

// vteStatus preserves the distinctions clients branch on: only a
// confirmed-absent emulator may be replaced, so ErrVTENotFound must
// survive the transport as a typed code, and a workspace without
// terminals is reported the way a server without the service is.
func vteStatus(err error) error {
	switch {
	case errors.Is(err, workspace.ErrVTENotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, workspace.ErrVTEUnsupported):
		return status.Error(codes.Unimplemented, err.Error())
	}
	return err
}
