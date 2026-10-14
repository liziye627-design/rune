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
	"sync"

	"google.golang.org/grpc"
	"unstable.build/rune/internal/runenet"
)

// Host is the node the server fronts. It is satisfied by
// [runenet.Node] and by anything that wraps one.
type Host interface {
	Status(ctx context.Context) (runenet.Status, error)
	Peers(ctx context.Context) ([]runenet.Peer, error)
	Join(ctx context.Context) error
	Up(ctx context.Context) error
	Rejoin(ctx context.Context) error
	Down(ctx context.Context) error
}

// Server serves a Host over gRPC and fans the host's changes out to
// every Watch stream.
type Server struct {
	UnimplementedMeshServer
	host    Host
	removed func(context.Context) bool

	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// NewServer fronts host. removed reports whether the account's machine
// list no longer has this machine in it; it is read for every Event and
// may be nil when nothing tracks that.
//
// It panics if host is nil: the server is registered by the process
// that owns the node, so a missing one is a programming error.
func NewServer(host Host, removed func(context.Context) bool) *Server {
	if host == nil {
		panic("meshrpc.NewServer: host must not be nil")
	}
	if removed == nil {
		removed = func(context.Context) bool { return false }
	}
	return &Server{
		host:    host,
		removed: removed,
		subs:    make(map[chan struct{}]struct{}),
	}
}

// Register installs the Mesh service on grpcServer.
func (s *Server) Register(grpcServer grpc.ServiceRegistrar) {
	RegisterMeshServer(grpcServer, s)
}

// Notify tells every Watch stream that the host changed. It never
// blocks: a stream still sending the previous event picks the change
// up with its next one, so a burst of changes collapses into as many
// events as the stream can carry.
func (s *Server) Notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case sub <- struct{}{}:
		default:
		}
	}
}

func (s *Server) subscribe() chan struct{} {
	sub := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	return sub
}

func (s *Server) unsubscribe(sub chan struct{}) {
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
}

// Status satisfies MeshServer.
func (s *Server) Status(
	ctx context.Context, _ *StatusRequest,
) (*StatusResponse, error) {
	st, err := s.host.Status(ctx)
	if err != nil {
		return nil, StatusError(err)
	}
	return statusToProto(st), nil
}

// Peers satisfies MeshServer.
func (s *Server) Peers(
	ctx context.Context, _ *PeersRequest,
) (*PeersResponse, error) {
	peers, err := s.host.Peers(ctx)
	if err != nil {
		return nil, StatusError(err)
	}
	return &PeersResponse{Peers: peersToProto(peers)}, nil
}

// Join satisfies MeshServer.
func (s *Server) Join(ctx context.Context, _ *JoinRequest) (*JoinResponse, error) {
	if err := s.host.Join(ctx); err != nil {
		return nil, StatusError(err)
	}
	return &JoinResponse{}, nil
}

// Up satisfies MeshServer.
func (s *Server) Up(ctx context.Context, _ *UpRequest) (*UpResponse, error) {
	if err := s.host.Up(ctx); err != nil {
		return nil, StatusError(err)
	}
	return &UpResponse{}, nil
}

// Rejoin satisfies MeshServer.
func (s *Server) Rejoin(ctx context.Context, _ *RejoinRequest) (*RejoinResponse, error) {
	if err := s.host.Rejoin(ctx); err != nil {
		return nil, StatusError(err)
	}
	return &RejoinResponse{}, nil
}

// Down satisfies MeshServer.
func (s *Server) Down(ctx context.Context, _ *DownRequest) (*DownResponse, error) {
	if err := s.host.Down(ctx); err != nil {
		return nil, StatusError(err)
	}
	return &DownResponse{}, nil
}

// Watch satisfies MeshServer. The stream ends without error when the
// client goes away, and with the host's error when the host cannot
// report its status: a client that wants to keep watching resubscribes.
func (s *Server) Watch(
	_ *WatchRequest, stream grpc.ServerStreamingServer[Event],
) error {
	sub := s.subscribe()
	defer s.unsubscribe(sub)
	ctx := stream.Context()
	for {
		ev, err := s.event(ctx)
		if err != nil {
			return StatusError(err)
		}
		if err := stream.Send(ev); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-sub:
		}
	}
}

// event is the host's current picture. A peer list the host cannot
// produce is reported empty rather than ending the stream: a node that
// is down has no peers, and that is a state worth watching through.
func (s *Server) event(ctx context.Context) (*Event, error) {
	st, err := s.host.Status(ctx)
	if err != nil {
		return nil, err
	}
	peers, err := s.host.Peers(ctx)
	if err != nil {
		peers = nil
	}
	return &Event{
		Status:         statusToProto(st),
		Peers:          peersToProto(peers),
		MachineRemoved: s.removed(ctx),
	}, nil
}
