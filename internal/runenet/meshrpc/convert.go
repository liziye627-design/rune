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

// Package meshrpc is the repository-private RPC surface the workspace
// server exposes its network node through, so the Rune clients on the
// same machine can read and drive the mesh membership the server
// owns.
package meshrpc

import (
	"context"
	"errors"
	"net/netip"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"unstable.build/rune/internal/runenet"
)

// statusToProto converts a node status to its wire form.
func statusToProto(st runenet.Status) *StatusResponse {
	return &StatusResponse{
		Hostname:   st.Hostname,
		Addrs:      addrsToProto(st.Addrs),
		ControlUrl: st.ControlURL,
		State:      st.State,
		AuthUrl:    st.AuthURL,
		LoginName:  st.LoginName,
		LastError:  st.LastError,
		MachineId:  st.MachineID,
	}
}

// statusFromProto converts a wire status back. Addresses that do not
// parse are dropped rather than failing the whole status: the status
// is diagnostic, and a bad address is one line of it.
func statusFromProto(st *StatusResponse) runenet.Status {
	if st == nil {
		return runenet.Status{}
	}
	return runenet.Status{
		Hostname:   st.GetHostname(),
		Addrs:      addrsFromProto(st.GetAddrs()),
		ControlURL: st.GetControlUrl(),
		State:      st.GetState(),
		AuthURL:    st.GetAuthUrl(),
		LoginName:  st.GetLoginName(),
		LastError:  st.GetLastError(),
		MachineID:  st.GetMachineId(),
	}
}

// peersToProto converts a peer list to its wire form.
func peersToProto(peers []runenet.Peer) []*Peer {
	ret := make([]*Peer, 0, len(peers))
	for _, p := range peers {
		ret = append(ret, &Peer{
			Hostname: p.Hostname,
			DnsName:  p.DNSName,
			Addrs:    addrsToProto(p.Addrs),
			Os:       p.OS,
			Online:   p.Online,
		})
	}
	return ret
}

// peersFromProto converts a wire peer list back.
func peersFromProto(peers []*Peer) []runenet.Peer {
	ret := make([]runenet.Peer, 0, len(peers))
	for _, p := range peers {
		ret = append(ret, runenet.Peer{
			Hostname: p.GetHostname(),
			DNSName:  p.GetDnsName(),
			Addrs:    addrsFromProto(p.GetAddrs()),
			OS:       p.GetOs(),
			Online:   p.GetOnline(),
		})
	}
	return ret
}

func addrsToProto(addrs []netip.Addr) []string {
	ret := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ret = append(ret, a.String())
	}
	return ret
}

func addrsFromProto(addrs []string) []netip.Addr {
	if len(addrs) == 0 {
		return nil
	}
	ret := make([]netip.Addr, 0, len(addrs))
	for _, s := range addrs {
		if a, err := netip.ParseAddr(s); err == nil {
			ret = append(ret, a)
		}
	}
	return ret
}

// sentinels pairs each runenet sentinel with the detail that names it
// on the wire and the gRPC code it travels under.
var sentinels = []struct {
	err  error
	kind ErrorDetail_Kind
	code codes.Code
}{
	{runenet.ErrNotStarted, ErrorDetail_NOT_STARTED, codes.FailedPrecondition},
	{runenet.ErrNotAuthenticated, ErrorDetail_NOT_AUTHENTICATED, codes.Unauthenticated},
	{runenet.ErrSubscriptionRequired, ErrorDetail_SUBSCRIPTION_REQUIRED, codes.PermissionDenied},
	{runenet.ErrMachineLimit, ErrorDetail_MACHINE_LIMIT, codes.ResourceExhausted},
	{runenet.ErrPeerNotFound, ErrorDetail_PEER_NOT_FOUND, codes.NotFound},
	{runenet.ErrMachineRemoved, ErrorDetail_MACHINE_REMOVED, codes.PermissionDenied},
}

// Kind names the runenet sentinel err wraps, or NONE.
func Kind(err error) ErrorDetail_Kind {
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return s.kind
		}
	}
	return ErrorDetail_NONE
}

// Sentinel is the runenet sentinel kind names, or nil for NONE.
func Sentinel(kind ErrorDetail_Kind) error {
	for _, s := range sentinels {
		if s.kind == kind {
			return s.err
		}
	}
	return nil
}

// StatusError wraps err in a gRPC status whose detail names the
// runenet sentinel it wraps, so [ErrorFromStatus] can restore it on the
// other side. Errors that wrap no sentinel travel as Unknown; a nil err
// stays nil. An error that is already a gRPC status is passed through,
// so a handler cannot bury a code it chose on purpose.
func StatusError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	for _, s := range sentinels {
		if !errors.Is(err, s.err) {
			continue
		}
		st, detailErr := status.New(s.code, err.Error()).
			WithDetails(&ErrorDetail{Kind: s.kind})
		if detailErr != nil {
			return status.Error(s.code, err.Error())
		}
		return st.Err()
	}
	return status.Error(codes.Unknown, err.Error())
}

// ErrorFromStatus maps a gRPC status carrying an [ErrorDetail] back to
// an error wrapping the sentinel it names, keeping the status message.
// A cancelled or expired context on the server comes back wrapping the
// context error. Every other error is returned as is, so callers can
// still inspect a transport failure's code.
func ErrorFromStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.Canceled:
		return RemoteError(st.Message(), context.Canceled)
	case codes.DeadlineExceeded:
		return RemoteError(st.Message(), context.DeadlineExceeded)
	}
	for _, d := range st.Details() {
		detail, ok := d.(*ErrorDetail)
		if !ok {
			continue
		}
		if sentinel := Sentinel(detail.GetKind()); sentinel != nil {
			return RemoteError(st.Message(), sentinel)
		}
	}
	return err
}

// RemoteError is an error another process reported as msg, which
// already names sentinel, so it reads exactly as it did there while
// errors.Is still matches sentinel here.
func RemoteError(msg string, sentinel error) error {
	return &remoteError{msg: msg, err: sentinel}
}

type remoteError struct {
	msg string
	err error
}

func (e *remoteError) Error() string { return e.msg }

func (e *remoteError) Unwrap() error { return e.err }
