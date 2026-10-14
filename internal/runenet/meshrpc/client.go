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

	"google.golang.org/grpc"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/runenet"
)

// Client drives the mesh node a workspace server hosts. Its methods
// mirror [runenet.Node]'s and return the same sentinels, so code that
// branches on them with errors.Is works unchanged against a node in
// another process.
type Client struct {
	rpc MeshClient
}

// NewClient returns a client over conn.
func NewClient(conn grpc.ClientConnInterface) *Client {
	return &Client{rpc: NewMeshClient(conn)}
}

// Change is one Watch notification: the node's status and peers at
// that moment, and whether the account has removed this machine.
type Change struct {
	Status         runenet.Status
	Peers          []runenet.Peer
	MachineRemoved bool
}

// Status mirrors [runenet.Node.Status].
func (c *Client) Status(ctx context.Context) (runenet.Status, error) {
	resp, err := c.rpc.Status(ctx, &StatusRequest{})
	if err != nil {
		return runenet.Status{}, ErrorFromStatus(err)
	}
	return statusFromProto(resp), nil
}

// Peers mirrors [runenet.Node.Peers].
func (c *Client) Peers(ctx context.Context) ([]runenet.Peer, error) {
	resp, err := c.rpc.Peers(ctx, &PeersRequest{})
	if err != nil {
		return nil, ErrorFromStatus(err)
	}
	return peersFromProto(resp.GetPeers()), nil
}

// Join mirrors [runenet.Node.Join].
func (c *Client) Join(ctx context.Context) error {
	_, err := c.rpc.Join(ctx, &JoinRequest{})
	return ErrorFromStatus(err)
}

// Up mirrors [runenet.Node.Up].
func (c *Client) Up(ctx context.Context) error {
	_, err := c.rpc.Up(ctx, &UpRequest{})
	return ErrorFromStatus(err)
}

// Rejoin mirrors [runenet.Node.Rejoin].
func (c *Client) Rejoin(ctx context.Context) error {
	_, err := c.rpc.Rejoin(ctx, &RejoinRequest{})
	return ErrorFromStatus(err)
}

// Down mirrors [runenet.Node.Down].
func (c *Client) Down(ctx context.Context) error {
	_, err := c.rpc.Down(ctx, &DownRequest{})
	return ErrorFromStatus(err)
}

// Watch subscribes to the node's changes. The channel carries one
// Change per server notification and is closed when the stream ends,
// whether because ctx was cancelled or because the server went away;
// the caller resubscribes if it still cares. The first Change arrives
// as soon as the server answers.
func (c *Client) Watch(ctx context.Context) (<-chan Change, error) {
	stream, err := c.rpc.Watch(ctx, &WatchRequest{})
	if err != nil {
		return nil, ErrorFromStatus(err)
	}
	ch := make(chan Change)
	go debug.CapturePanicReport(func() {
		defer close(ch)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			ev := Change{
				Status:         statusFromProto(msg.GetStatus()),
				Peers:          peersFromProto(msg.GetPeers()),
				MachineRemoved: msg.GetMachineRemoved(),
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	})
	return ch, nil
}
