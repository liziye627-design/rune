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

package main

import (
	"context"
	"io"
	"os"

	log "github.com/sirupsen/logrus"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace/vtescheme"
)

// runVTEBridge joins this process's standard input and output to the
// terminal attachment socket of this machine's workspace server.
//
// It is what lets a Rune on another host attach to these terminals:
// the attachment protocol is a raw byte stream, so an ssh channel
// running this carries it unchanged. The server is started here
// exactly as it is for a local client, so attaching over ssh and
// attaching locally reach the same terminals; the attachment names
// its workspace itself.
func runVTEBridge() int {
	conn, err := vtescheme.DialAttachment(context.Background(),
		vtescheme.ClientOptions{
			DataDir: *flagDataPath,
			Command: workspaceServerCommand(*flagDataPath, *flagConfigPath),
		})
	if err != nil {
		log.Errorf("dial terminal attachment: %v", err)
		return 3
	}
	defer func() { _ = conn.Close() }()

	// Either direction ending ends the bridge: a detached client and a
	// closed attachment are both reasons to stop copying.
	done := make(chan struct{}, 2)
	go debug.CapturePanicReport(func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(conn, os.Stdin)
	})
	go debug.CapturePanicReport(func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(os.Stdout, conn)
	})
	<-done
	return 0
}
