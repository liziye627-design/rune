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

	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspacerpc/vterpc"
)

// VTEClient reaches the terminal lifecycle of a remote workspace
// server: which terminals of one workspace exist, and terminating one.
//
// Attaching to a terminal is not here. An attachment is a raw byte
// stream between a client's emulator and a pty, so it has a transport
// of its own (see workspace/vtewire).
type VTEClient struct {
	c    vterpc.VTEClient
	root string
}

// NewVTEClient allocates storage for a new VTEClient and initializes
// it with conn and the workspace it is scoped to. An empty root is the
// server's own.
func NewVTEClient(conn grpc.ClientConnInterface, root string) *VTEClient {
	return &VTEClient{c: vterpc.NewVTEClient(conn), root: root}
}

// ListVTEs satisfies workspace.VTEOwner.
func (c *VTEClient) ListVTEs(ctx context.Context) ([]workspace.VTEInfo, error) {
	resp, err := c.c.List(ctx, &vterpc.ListRequest{Root: c.root})
	if err != nil {
		return nil, vteError(err)
	}
	ret := make([]workspace.VTEInfo, 0, len(resp.GetVtes()))
	for _, info := range resp.GetVtes() {
		ret = append(ret, info.ToModel())
	}
	return ret, nil
}

// DeleteVTE satisfies workspace.VTEOwner.
func (c *VTEClient) DeleteVTE(ctx context.Context, id workspace.VTEID) error {
	_, err := c.c.Delete(ctx, &vterpc.DeleteRequest{Id: string(id), Root: c.root})
	return vteError(err)
}

// vteError restores the sentinels the status codes stand for: NotFound
// keeps the snapshot fallback firing only on a confirmed missing
// emulator, and Unimplemented is a server whose scheme owns no
// terminals at all.
func vteError(err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.NotFound:
		return workspace.ErrVTENotFound
	case codes.Unimplemented:
		return workspace.ErrVTEUnsupported
	}
	return err
}
