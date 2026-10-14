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

// Command runesvc is a stripped-down stand-in for the `rune` workspace
// server (see cmd/rune/main.go's --workspace-server / -x flag).
//
// It exists solely to give the SSH e2e tests a Linux binary they can
// install as `rune` inside the test container, exercising the full
// connectScheme path (whichCommand + workspaceExists + StartSchemeServer)
// without having to cross-compile the full rune binary (which depends on
// CGO and GUI libraries that don't cross-compile cleanly from a dev
// laptop).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/ide/hostenv"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspaced"
	"unstable.build/rune/internal/workspace/workspacerpc"
	"unstable.build/rune/internal/workspace/workspaceserver"
	"unstable.build/rune/internal/workspace/workspacessh"
)

func main() {
	workspacePath := flag.String("x", "",
		"local workspace path to expose over the workspace gRPC server")
	headless := flag.Bool("headless", false,
		"run this machine's workspace server, mirroring cmd/rune")
	daemon := flag.Bool("daemon", false,
		"exit quietly when another workspace server holds the lock")
	dataDir := flag.String("datadir", "",
		"data directory used by the workspace server")
	vteBridge := flag.Bool("vte-bridge", false,
		"bridge stdio to the terminal attachment socket of this "+
			"machine's workspace server, starting it if none is")
	flag.Parse()

	home := resolveDataDir(*dataDir)
	if *headless {
		os.Exit(runHeadless(home, *daemon))
	}
	if *vteBridge {
		os.Exit(runVTEBridge(home))
	}

	if *workspacePath == "" {
		fmt.Fprintln(os.Stderr, "runesvc: -x <workspace-path> is required")
		os.Exit(2)
	}

	uri, err := workspaceapi.ParseURI("file://" + *workspacePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: parse uri:", err)
		os.Exit(3)
	}
	if *dataDir == "" {
		*dataDir = defaultDataDir()
	}
	// Set up the host environment the way `rune -x` does, so terminals
	// started here exercise the real shell rc and gui.env mechanism.
	shellRCDir, err := workspace.InstallShellRC(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: install shell rc:", err)
	}
	if err := hostenv.New(*dataDir, shellRCDir).Apply(guiEnv(*dataDir)); err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: apply host environment:", err)
	}
	scheme, err := workspace.NewFileSchemeFunc(*dataDir, shellRCDir)(
		context.Background(), config.NopConfig(), uri)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: new file scheme:", err)
		os.Exit(4)
	}
	// Same split as cmd/rune: this server dies with the ssh transport,
	// so it only reaches the machine's workspace server, which owns the
	// terminals.
	client := vtescheme.NewClient(context.Background(), scheme, uri,
		clientOptions(home))
	defer client.Close()

	server := workspacerpc.NewServer(client,
		workspacerpc.CommandAuthorizerFunc(
			func(context.Context, workspaceapi.Cmd) error { return nil }))
	defer func() { _ = server.Stop() }()

	// Stand in for a slow pre-serving phase (e.g. a slow config load). The
	// delay happens before StartSchemeServer emits the ServerReady sentinel,
	// so an e2e test can prove connectScheme waits for readiness and does not
	// hand back a client while stdout carries no gRPC server yet.
	sleepServeDelay(*dataDir)

	grpcServer := workspacessh.NewSchemeServer()
	if err := servePackages(grpcServer, filepath.Join(*dataDir, "bin")); err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: serve packages:", err)
		os.Exit(6)
	}
	if err := workspacessh.StartSchemeServer(
		log.New(), server, grpcServer); err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: start scheme server:", err)
		os.Exit(5)
	}
}

// clientOptions starts this machine's workspace server the way cmd/rune
// does: the same binary, asked to run headless as a daemon.
func clientOptions(dataDir string) vtescheme.ClientOptions {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: resolve executable:", err)
		os.Exit(5)
	}
	return vtescheme.ClientOptions{
		DataDir: dataDir,
		Command: []string{self, "-headless", "-daemon", "-datadir", dataDir},
	}
}

// runHeadless serves every workspace on this machine from one process,
// mirroring cmd/rune's --headless.
func runHeadless(dataDir string, daemon bool) int {
	uri, err := workspaceapi.CurrentUserHostURI("/")
	if err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: root uri:", err)
		return 2
	}
	scheme, err := workspace.NewFileScheme(
		context.Background(), config.NopConfig(), uri)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: root scheme:", err)
		return 3
	}
	server, err := workspaced.Start(workspaced.Options{
		DataDir: dataDir,
		Scheme:  scheme,
	})
	if daemon && errors.Is(err, workspaceserver.ErrLocked) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runesvc:", err)
		return 1
	}
	defer func() { _ = server.Close() }()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigs:
	case <-server.Done():
	}
	return 0
}

func resolveDataDir(dataDir string) string {
	if dataDir != "" {
		return dataDir
	}
	return defaultDataDir()
}

// sleepServeDelay blocks for the duration recorded in the remote data
// directory's serve_delay file, if present. It lets an e2e test inject a
// deterministic pre-serving delay without a test-only flag on the production
// connectScheme launch. A missing or malformed file is a no-op.
func sleepServeDelay(dataDir string) {
	if dataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "serve_delay"))
	if err != nil {
		return
	}
	d, err := time.ParseDuration(strings.TrimSpace(string(data)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "runesvc: parse serve_delay: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "runesvc: delaying serve by %s\n", d)
	time.Sleep(d)
}

// guiEnv stands in for the gui.env block `rune -x` reads from the user
// config: KEY=VALUE lines in the data directory's gui_env file, if present.
func guiEnv(dataDir string) config.Config {
	env := map[string]any{}
	data, err := os.ReadFile(filepath.Join(dataDir, "gui_env"))
	if err != nil {
		return config.MapConfig(env)
	}
	for line := range strings.Lines(string(data)) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			env[k] = v
		}
	}
	return config.MapConfig(env)
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if u, uerr := user.Current(); uerr == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".rune")
}

// runVTEBridge joins stdio to the attachment socket of this machine's
// workspace server, mirroring cmd/rune's --vte-bridge.
func runVTEBridge(dataDir string) int {
	conn, err := vtescheme.DialAttachment(context.Background(),
		clientOptions(dataDir))
	if err != nil {
		fmt.Fprintln(os.Stderr, "runesvc: dial terminal attachment:", err)
		return 3
	}
	defer func() { _ = conn.Close() }()

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
