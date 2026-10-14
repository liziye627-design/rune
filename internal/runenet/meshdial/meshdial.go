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

// Package meshdial carries connections to Rune network peers over a
// unix socket, so a process that does not host the machine's network
// node — a Rune editor — can still reach the machines on it through
// the workspace server that does.
//
// A client opens the socket, names the peer in one line, and from the
// server's one-line answer on is talking to the peer: the server copies
// bytes both ways and closes each side when the other ends. The result
// is a plain net.Conn, which is what the rune:// scheme already expects
// from a node in its own process.
package meshdial

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
	"github.com/unstablebuild/blue/logging"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/runenet/meshrpc"
)

// Verbs a client opens with. Each names what on the peer the stream
// should reach: its workspace port or its terminal attachment port.
const (
	verbDial   = "DIAL"
	verbAttach = "ATTACH"
)

const (
	replyOK  = "OK"
	replyErr = "ERR"
)

// handshakeTimeout bounds how long the server waits for a client's
// opening line, so a client that connects and says nothing cannot hold
// a goroutine forever.
const handshakeTimeout = 10 * time.Second

// dialTimeout bounds the server's dial of the peer. The client's own
// context is not visible from here, so without it a client that gave
// up on an unreachable peer would leave the dial running until the
// network stack gave up too.
const dialTimeout = 30 * time.Second

// maxLine bounds a handshake line. Peer names are hostnames or
// addresses; anything longer is not a handshake.
const maxLine = 1024

// PeerDialer reaches peers on the mesh. It is satisfied by
// [runenet.Node].
type PeerDialer interface {
	Dial(ctx context.Context, peer string) (net.Conn, error)
	DialAttachment(ctx context.Context, peer string) (net.Conn, error)
}

// Serve accepts connections on lis until lis is closed, dialing the
// named peer through dialer for each. ctx bounds every dial but does
// not unblock Accept, so the caller closes lis to stop serving. Serve
// returns nil once lis is closed and the accept error otherwise.
func Serve(ctx context.Context, lis net.Listener, dialer PeerDialer) error {
	if dialer == nil {
		panic("meshdial.Serve: dialer must not be nil")
	}
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go debug.CapturePanicReport(func() {
			if err := serveConn(ctx, conn, dialer); err != nil {
				log.WithField(logging.KeyClass, "meshdial").
					Debugf("mesh dial: %v", err)
			}
		})
	}
}

// serveConn runs one client's handshake and, when the peer answers,
// proxies until either side ends.
func serveConn(ctx context.Context, client net.Conn, dialer PeerDialer) error {
	defer func() { _ = client.Close() }()

	_ = client.SetReadDeadline(time.Now().Add(handshakeTimeout))
	// Bytes the client sends after its line stay in this reader, so
	// it is what the proxy copies from.
	br := bufio.NewReaderSize(client, maxLine)
	verb, peer, err := readRequest(br)
	if err != nil {
		_ = writeReply(client, replyErr, meshrpc.ErrorDetail_NONE, err.Error())
		return err
	}
	_ = client.SetReadDeadline(time.Time{})

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var peerConn net.Conn
	switch verb {
	case verbDial:
		peerConn, err = dialer.Dial(dialCtx, peer)
	case verbAttach:
		peerConn, err = dialer.DialAttachment(dialCtx, peer)
	}
	if err != nil {
		_ = writeReply(client, replyErr, meshrpc.Kind(err), err.Error())
		return fmt.Errorf("%s %s: %w", strings.ToLower(verb), peer, err)
	}
	defer func() { _ = peerConn.Close() }()
	if err := writeReply(client, replyOK, meshrpc.ErrorDetail_NONE, ""); err != nil {
		return err
	}
	proxy(br, client, peerConn)
	return nil
}

// readRequest parses the opening line: a verb and a peer name.
func readRequest(br *bufio.Reader) (verb, peer string, err error) {
	// ReadSlice stops at the reader's size, which is what bounds a
	// client that never sends a newline.
	raw, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", "", errors.New("handshake line too long")
		}
		return "", "", fmt.Errorf("read handshake: %w", err)
	}
	line := string(raw)
	verb, peer, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
	if !ok || (verb != verbDial && verb != verbAttach) {
		return "", "", fmt.Errorf("malformed handshake %q", strings.TrimSpace(line))
	}
	if err := validPeer(peer); err != nil {
		return "", "", err
	}
	return verb, peer, nil
}

// validPeer rejects names the line framing cannot carry.
func validPeer(peer string) error {
	if peer == "" {
		return errors.New("empty peer name")
	}
	if strings.ContainsAny(peer, " \t\r\n") {
		return fmt.Errorf("peer name %q contains whitespace", peer)
	}
	return nil
}

// writeReply sends one answer line. Error messages are flattened to a
// single line so the framing survives whatever the dial reported.
func writeReply(w io.Writer, reply string, kind meshrpc.ErrorDetail_Kind, msg string) error {
	var line string
	if reply == replyOK {
		line = replyOK + "\n"
	} else {
		msg = strings.NewReplacer("\r", " ", "\n", " ").Replace(msg)
		line = fmt.Sprintf("%s %s %s\n", replyErr, kind.String(), msg)
	}
	_, err := io.WriteString(w, line)
	return err
}

// proxy copies bytes between the client and the peer until one side
// ends, then closes the other so the end is observed on both.
func proxy(clientReader io.Reader, client, peer net.Conn) {
	done := make(chan struct{}, 2)
	go debug.CapturePanicReport(func() {
		_, _ = io.Copy(peer, clientReader)
		_ = peer.Close()
		done <- struct{}{}
	})
	go debug.CapturePanicReport(func() {
		_, _ = io.Copy(client, peer)
		_ = client.Close()
		done <- struct{}{}
	})
	<-done
	<-done
}

// Dialer reaches mesh peers through the server on Socket. It satisfies
// workspacerune.Dialer, so the rune:// scheme dials a node in another
// process exactly as it dials one in its own.
type Dialer struct {
	// Socket is the server's network socket; see
	// workspaceserver.NetSocketPath.
	Socket string
}

// Dial opens a connection to peer's workspace port.
func (d Dialer) Dial(ctx context.Context, peer string) (net.Conn, error) {
	return d.dial(ctx, verbDial, peer)
}

// DialAttachment opens a connection to peer's terminal attachment port.
func (d Dialer) DialAttachment(ctx context.Context, peer string) (net.Conn, error) {
	return d.dial(ctx, verbAttach, peer)
}

func (d Dialer) dial(ctx context.Context, verb, peer string) (net.Conn, error) {
	if d.Socket == "" {
		return nil, errors.New("meshdial: socket is required")
	}
	if err := validPeer(peer); err != nil {
		return nil, err
	}
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "unix", d.Socket)
	if err != nil {
		return nil, fmt.Errorf("dial mesh socket: %w", err)
	}
	if err := d.handshake(ctx, conn, verb, peer); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// handshake sends the request line and reads the answer, giving up
// when ctx ends first.
func (d Dialer) handshake(ctx context.Context, conn net.Conn, verb, peer string) error {
	// A handshake cannot be interrupted by ctx on its own; closing the
	// connection under it is what makes the blocked read return.
	expired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(expired)
	})
	line, err := exchange(conn, verb, peer)
	if !stop() {
		<-expired
		if err == nil {
			// ctx ended as the reply arrived; the deadline it set
			// must not outlive the handshake on a stream that works.
			_ = conn.SetDeadline(time.Time{})
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return parseReply(line)
}

// exchange sends the request line and reads the reply line.
func exchange(conn net.Conn, verb, peer string) (string, error) {
	if _, err := fmt.Fprintf(conn, "%s %s\n", verb, peer); err != nil {
		return "", fmt.Errorf("send handshake: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		return "", fmt.Errorf("read handshake reply: %w", err)
	}
	return line, nil
}

// readLine reads one line a byte at a time: a buffered read could take
// the first bytes the peer sends after the reply, and they belong to
// the stream handed to the caller.
func readLine(r io.Reader) (string, error) {
	line := make([]byte, 0, 64)
	var b [1]byte
	for len(line) < maxLine {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", errors.New("line too long")
}

// parseReply maps an answer line to the error it names, or nil for OK.
// A kind naming a runenet sentinel is restored as that sentinel so
// errors.Is works across the socket.
func parseReply(line string) error {
	if line == replyOK {
		return nil
	}
	reply, rest, _ := strings.Cut(line, " ")
	if reply != replyErr {
		return fmt.Errorf("malformed handshake reply %q", line)
	}
	kindName, msg, _ := strings.Cut(rest, " ")
	kind := meshrpc.ErrorDetail_Kind(meshrpc.ErrorDetail_Kind_value[kindName])
	if sentinel := meshrpc.Sentinel(kind); sentinel != nil {
		return meshrpc.RemoteError(msg, sentinel)
	}
	if msg == "" {
		msg = rest
	}
	return errors.New(msg)
}
