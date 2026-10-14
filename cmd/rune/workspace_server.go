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
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	log "github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
	"github.com/unstablebuild/blue/logging"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"google.golang.org/grpc"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/ide/hostenv"
	"unstable.build/rune/internal/ide/idepkg/pkgrpc"
	"unstable.build/rune/internal/llm/llmrpc"
	"unstable.build/rune/internal/rpc"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspaced"
	"unstable.build/rune/internal/workspace/workspacerpc"
	"unstable.build/rune/internal/workspace/workspacessh"
)

// runeExecutable is the binary a workspace server is started from. It
// is this process: only rune knows it is rune, and a workspace server
// must not be started from a binary that merely embeds the IDE.
func runeExecutable() string {
	path, err := os.Executable()
	if err != nil {
		log.Errorf("resolve rune binary: %v", err)
	}
	return path
}

// workspaceServerCommand starts this machine's workspace server on
// demand. It is the same `rune --headless` an operator runs, told it
// was spawned so that losing the start race to another server is not
// an error: whoever spawned it connects to the winner.
//
// The server joins the network for the editor's account, so it is
// pointed at the Rune API this process was pointed at.
func workspaceServerCommand(dataDir, configPath string) []string {
	ret := []string{
		runeExecutable(), "--headless", "--daemon",
		"--datadir", dataDir, "--config", configPath,
	}
	for _, name := range []string{
		"rune-http-address", "rune-grpc-address", "rune-grpc-insecure",
		"rune-release-collection", "rune-website-address",
	} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != f.DefValue {
			ret = append(ret, "--"+name+"="+f.Value.String())
		}
	}
	return ret
}

// startServerLog points the log at the workspace server log: nobody
// reads the output of a server that runs over ssh or detached from any
// terminal. debug.StartPProfOnSignal writes the pprof HTTP address here
// on SIGUSR1.
func startServerLog() (func(), error) {
	// The log defaults to <default datadir>/server.log; when the
	// datadir is overridden without an explicit --workspace-server-log,
	// it stays beside the rest of the server state.
	path := *flagWorkspaceServerLogFile
	if !flag.Lookup("workspace-server-log").Changed {
		path = filepath.Join(*flagDataPath, "server.log")
	}
	f, err := workspace.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("open server log: %w", err)
	}
	log.SetOutput(f)
	log.SetLevel(log.InfoLevel)
	log.SetFormatter(logging.LogrusLogdFormatter{})
	rpc.EnableGRPCLogging(f, f, f)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	return func() {
		_ = f.Sync()
		_ = f.Close()
	}, nil
}

func workspaceServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(rpc.UnaryReportRecoveryInterceptor()),
		grpc.ChainStreamInterceptor(rpc.StreamReportRecoveryInterceptor()),
		grpc.MaxRecvMsgSize(llmrpc.MaxRecvMsgSize),
		grpc.MaxSendMsgSize(llmrpc.MaxSendMsgSize),
	}
}

// startWorkspaceServer serves one workspace over standard input and
// output, which is how a Rune on another host reaches this one over
// ssh.
func startWorkspaceServer(host *hostenv.Host, shellRCDir string, shellRCErr error) int {
	closeLog, err := startServerLog()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeLog()

	grpcServer := workspacessh.NewSchemeServer(workspaceServerOptions()...)
	defer stopOnSignal(grpcServer.Stop)()

	uri, err := workspaceapi.CurrentUserHostURI(*flagWorkspaceServer)
	if err != nil {
		log.Error(err)
		return 2
	}

	if shellRCErr != nil {
		reportRemoteShellRCErr(os.Stderr, shellRCErr)
	}
	inner, err := workspace.NewFileSchemeFunc(*flagDataPath, shellRCDir)(
		context.Background(), config.NopConfig(), uri)
	if err != nil {
		log.Error(err)
		return 3
	}
	// This server dies with the ssh transport that spawned it, so it
	// must not own terminals. It reaches this machine's workspace
	// server instead, which is what lets a remote terminal survive a
	// dropped connection.
	scheme := vtescheme.NewClient(context.Background(), inner, uri,
		vtescheme.ClientOptions{
			DataDir: *flagDataPath,
			Command: workspaceServerCommand(*flagDataPath, *flagConfigPath),
		})
	defer scheme.Close()
	// Apply gui.env before serving so the tools the client starts here
	// see the packages installed on this host.
	loadRemoteConfigAndApplyEnv(host, scheme, uri)

	// The client installs the packages its workspace needs on this host
	// through this manager, served next to the workspace.
	pkgs, pkgStorage := newHostPackageManager(newRuneStorage(*flagDataPath),
		newRemoteReleaseManager(), scheme,
		func() { loadRemoteConfigAndApplyEnv(host, scheme, uri) })
	defer func() {
		_ = pkgStorage.Close()
	}()
	pkgrpc.NewServer(pkgs).Register(grpcServer)

	server := workspacerpc.NewServer(scheme,
		workspacerpc.CommandAuthorizerFunc(
			func(context.Context, workspaceapi.Cmd) error { return nil }))
	defer func() {
		_ = server.Stop()
	}()

	err = workspacessh.StartSchemeServer(log.StandardLogger(), server, grpcServer)
	if err != nil {
		log.Error(err)
		return 4
	}
	log.Tracef("StartSchemeServer returned with no error")
	return 0
}

// stopOnSignal calls stop when the process is asked to terminate, and
// logs every other signal for debugging. The returned function stops
// watching.
func stopOnSignal(stop func()) func() {
	ch := make(chan os.Signal, 1)
	quit := make(chan struct{})
	signal.Notify(ch)
	go debug.CapturePanicReport(func() {
		defer stop()
		for {
			select {
			case sig := <-ch:
				switch sig {
				case syscall.SIGTERM, syscall.SIGINT:
					log.Infof("Received %v signal: cleaning up...", sig)
					return
				default:
					if isUrgentDataSignal(sig) {
						break
					}
					log.Debugf("Received unhandled signal: %#v", sig)
				}
			case <-quit:
				return
			}
		}
	})
	return func() {
		signal.Reset()
		close(quit)
	}
}

// startMachineServer starts the workspace server of this machine: every
// workspace under "/" is served from one process, which is what keeps
// their terminals alive beyond any Rune client. opts carries what else
// this process serves and reports; its data directory, scheme and RPC
// options are this process's.
func startMachineServer(
	shellRCDir string, opts workspaced.Options,
) (*workspaced.Server, error) {
	uri, err := workspaceapi.CurrentUserHostURI("/")
	if err != nil {
		return nil, fmt.Errorf("root workspace URI: %w", err)
	}
	scheme, err := workspace.NewFileSchemeFunc(*flagDataPath, shellRCDir)(
		context.Background(), config.NopConfig(), uri)
	if err != nil {
		return nil, fmt.Errorf("root workspace scheme: %w", err)
	}
	opts.DataDir = *flagDataPath
	opts.Scheme = scheme
	opts.ServerOptions = workspaceServerOptions()
	return workspaced.Start(opts)
}
