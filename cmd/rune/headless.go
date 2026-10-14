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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/blue/logging"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/storageapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/auth"
	"unstable.build/rune/cmd/rune/ide/apiclient"
	"unstable.build/rune/internal/ide"
	"unstable.build/rune/internal/ide/hostenv"
	"unstable.build/rune/internal/runenet"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/workspaceserver"
)

// headlessClient is the subset of *apiclient.Client a headless node
// needs to get itself signed in.
type headlessClient interface {
	LoginWithDeviceCode(ctx context.Context) apiclient.DeviceLoginSession
	AccountStatus(ctx context.Context) (auth.RPCUser, bool, error)
	CheckSignIn(ctx context.Context) error
	Logout(ctx context.Context) error
}

// runHeadless runs this machine's workspace server with no editor at
// all: no IDE, no terminal UI and no window. It owns the terminals of
// every workspace on the machine, so they outlive every Rune client,
// and it is the one process that may serve them: a second one fails on
// the data directory's lock.
//
// Run by an operator, it also serves the machine on the Rune network
// when network.auto_join is set. Everything the operator would
// otherwise read out of the editor — the sign-in code, the account,
// the node's mesh status — goes to stdout, and the editor log is teed
// there too so the process is usable under a service manager.
//
// Spawned on demand by a Rune (--daemon), it logs to the server log
// and treats a lock another instance holds as the server it was asked
// to start: whoever spawned it connects to that one.
func runHeadless(
	ctx context.Context, host *hostenv.Host, shellRCDir string, shellRCErr error,
) int {
	rootCfg, err := ide.Config(*flagConfigPath, runeDefaultConfig())
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %s\n", err)
		return 1
	}

	var closeLog func()
	if *flagDaemon {
		closeLog, err = startServerLog()
	} else {
		closeLog, err = startHeadlessLogging(rootCfg, os.Stdout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return 1
	}
	defer closeLog()
	// No UI to notify: the log is teed to the operator's stdout.
	if shellRCErr != nil {
		log.Warn(shellRCErr)
	}

	netCfg, err := networkConfig(rootCfg, *flagDataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return 1
	}
	applyConfigEnv(host, rootCfg)

	// The socket answers before the server is done starting, so a
	// signal that lands in between must still stop it the clean way.
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	server, err := startMachineServer(shellRCDir)
	if err != nil {
		if *flagDaemon && errors.Is(err, workspaceserver.ErrLocked) {
			log.Infof("not starting a workspace server: %v", err)
			return 0
		}
		return headlessFailure(err)
	}
	defer func() {
		_ = server.Close()
	}()
	log.Infof("serving this machine's workspaces on %s",
		workspaceserver.SocketPath(*flagDataPath))

	switch {
	case *flagDaemon:
	case !netCfg.AutoJoin:
		// An operator reads this where a headless node used to refuse
		// to start: nothing else says why it never joins.
		log.Infof("network.auto_join is off in %s: serving this machine only, "+
			"not joining the Rune network", *flagConfigPath)
	default:
		closeNet, err := startHeadlessNetwork(ctx, rootCfg, host, shellRCDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			return 1
		}
		defer closeNet()
	}

	waitForShutdown(ctx, server.Done())
	// A second signal kills a server stuck cleaning up.
	stopSignals()
	if err := server.Err(); err != nil {
		return headlessFailure(fmt.Errorf("workspace server stopped: %w", err))
	}
	return 0
}

// headlessFailure reports why the server could not run where whoever
// started it looks: the server log for a spawned one, whose standard
// error nobody reads, and standard error for an operator's, whose log
// already goes to standard output.
func headlessFailure(err error) int {
	if *flagDaemon {
		log.Error(err)
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	return 1
}

// startHeadlessNetwork signs the machine in and joins the Rune network,
// serving this machine's workspaces to its peers until the returned
// function is called.
func startHeadlessNetwork(
	ctx context.Context, rootCfg config.Config, host *hostenv.Host, shellRCDir string,
) (func(), error) {
	storage := newRuneStorage(*flagDataPath)
	apicfg := apiClientConfig(os.TempDir(), rootCfg)
	apicfg.Headless = true
	client := apiclient.New(storage, apicfg, *flagDataPath)

	if err := headlessLogin(ctx, client, os.Stdout); err != nil {
		client.Close()
		return nil, fmt.Errorf("login: %w", err)
	}

	net := newNetwork(rootCfg, *flagDataPath, shellRCDir, newNetworkGate(client))
	closeNet := func() {
		_ = net.Close()
		client.Close()
	}

	closePackages, err := serveHeadlessPackages(ctx, net, storage, host, shellRCDir)
	if err != nil {
		closeNet()
		return nil, err
	}
	closeAll := func() {
		closePackages()
		closeNet()
	}

	if err := net.join(ctx); err != nil {
		closeAll()
		return nil, fmt.Errorf("could not join the network: %w", err)
	}

	st, err := net.node.Status(ctx)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("network status: %w", err)
	}
	fmt.Fprint(os.Stdout, formatNodeStatus(st))
	return closeAll, nil
}

// serveHeadlessPackages has peers install packages on this machine
// through a manager of its own: there is no editor to own them. The
// user config's gui.env is applied now and after every install that
// changes it, so commands peers start see the installed toolchains.
func serveHeadlessPackages(
	ctx context.Context, net *network, rootStorage storageapi.Service,
	host *hostenv.Host, shellRCDir string,
) (func(), error) {
	uri, err := workspaceapi.CurrentUserHostURI("/")
	if err != nil {
		return nil, fmt.Errorf("root workspace URI: %w", err)
	}
	scheme, err := workspace.NewFileSchemeFunc(*flagDataPath, shellRCDir)(
		ctx, config.NopConfig(), uri)
	if err != nil {
		return nil, fmt.Errorf("root workspace scheme: %w", err)
	}
	applyEnv := func() { applyUserConfigEnv(host) }
	applyEnv()
	pkgs, pkgStorage := newHostPackageManager(
		rootStorage, newRemoteReleaseManager(), scheme, applyEnv)
	net.packages.set(pkgs)
	return func() {
		_ = pkgStorage.Close()
		_ = scheme.Close()
	}, nil
}

func applyUserConfigEnv(host *hostenv.Host) {
	cfg, err := ide.Config(*flagConfigPath, runeDefaultConfig())
	if err != nil {
		log.Warnf("load config to apply gui.env: %v", err)
		if cfg == nil {
			return
		}
	}
	applyConfigEnv(host, cfg)
}

// startHeadlessLogging points the editor log at its configured file and
// tees it to extra, so an operator watching the process sees what the
// log file records. Without a configured log_path the log goes to extra
// only.
func startHeadlessLogging(
	rootCfg config.Config, extra io.Writer,
) (func(), error) {
	level := headlessLogLevel(rootCfg)
	log.SetLevel(level)
	log.SetFormatter(logging.LogrusLogdFormatter{})

	logPath, err := rootCfg.GetString("log_path")
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		return nil, fmt.Errorf("read log_path from config: %w", err)
	}
	logPath = os.ExpandEnv(logPath)
	if logPath == "" {
		log.SetOutput(extra)
		slog.SetDefault(slog.New(slog.NewTextHandler(extra, nil)))
		return func() {}, nil
	}

	f, err := workspace.OpenFile(logPath,
		os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", logPath, err)
	}
	out := io.MultiWriter(f, extra)
	log.SetOutput(out)
	slog.SetDefault(slog.New(slog.NewTextHandler(out, nil)))
	return func() {
		_ = f.Close()
	}, nil
}

func headlessLogLevel(rootCfg config.Config) log.Level {
	levelStr, err := rootCfg.GetString("log_level")
	if err != nil {
		return log.InfoLevel
	}
	level, err := log.ParseLevel(levelStr)
	if err != nil {
		return log.InfoLevel
	}
	return level
}

// headlessLogin signs the machine in as a serve-only machine when it is
// not already, printing the sign-in code to out and blocking until the
// operator enters it in a browser on whatever machine they are sitting
// at. A sign-in with full account access is never kept: whoever took
// the machine would hold the account.
func headlessLogin(
	ctx context.Context, client headlessClient, out io.Writer,
) error {
	user, ok, err := client.AccountStatus(ctx)
	if err != nil {
		return err
	}
	// Left over from before headless nodes were serve-only, or in a
	// data directory copied from a desktop install.
	if ok && !user.ServeOnly {
		fmt.Fprint(out, "This machine holds a sign-in with full account "+
			"access; signing it in again to serve only.\n\n")
		if err := client.Logout(ctx); err != nil {
			return fmt.Errorf("discard full-access sign-in: %w", err)
		}
		ok = false
	}
	// The cached copy cannot tell whether the sign-in has been revoked
	// or has expired since it was stored; only the server can, and only
	// when asked to refresh it.
	if ok {
		switch err := client.CheckSignIn(ctx); {
		case errors.Is(err, auth.ErrNotAuthenticated):
			fmt.Fprint(out, "This machine's sign-in was revoked or has "+
				"expired; signing it in again.\n\n")
			ok = false
		case err != nil:
			return fmt.Errorf("check sign-in: %w", err)
		}
	}
	if ok {
		fmt.Fprint(out, formatHeadlessAccount(user))
		return nil
	}

	session := client.LoginWithDeviceCode(ctx)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case prompt, ok := <-session.Prompt:
		if ok {
			fmt.Fprint(out, formatDevicePrompt(prompt))
		}
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-session.Done:
		if err != nil {
			return err
		}
	}

	user, ok, err = client.AccountStatus(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("login completed but no account token was stored")
	}
	if !user.ServeOnly {
		_ = client.Logout(ctx)
		return errors.New("the API server did not issue a serve-only " +
			"sign-in, so this machine cannot run headless; the sign-in " +
			"was discarded")
	}
	fmt.Fprint(out, formatHeadlessAccount(user))
	return nil
}

func formatDevicePrompt(p apiclient.DevicePrompt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "To sign this machine in, open\n\n    %s\n\n"+
		"in any browser and enter the code %s", p.VerificationURI, p.UserCode)
	if !p.Expiry.IsZero() {
		fmt.Fprintf(&b, " (expires %s)", p.Expiry.Local().Format("15:04"))
	}
	b.WriteString(".\n\n")
	return b.String()
}

func formatHeadlessAccount(u auth.RPCUser) string {
	var b strings.Builder
	b.WriteString("Signed in")
	if u.Email != "" {
		fmt.Fprintf(&b, " as %s", u.Email)
	}
	fmt.Fprintf(&b, " (%s)\n", headlessPlanLabel(u.Role))
	return b.String()
}

func headlessPlanLabel(role auth.Role) string {
	switch {
	case role == auth.RoleOneOff:
		return "One-off"
	case role >= auth.RolePaid:
		return "Rune Pro"
	default:
		return "Rune"
	}
}

func formatNodeStatus(st runenet.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Network node %s is %s\n", st.Hostname, st.State)
	for _, addr := range st.Addrs {
		fmt.Fprintf(&b, "  address: %s\n", addr)
	}
	if st.LastError != "" {
		fmt.Fprintf(&b, "  last error: %s\n", st.LastError)
	}
	return b.String()
}

// waitForShutdown blocks until the service manager (or the operator)
// asks the server to stop, the server stops on its own, or ctx is
// cancelled.
func waitForShutdown(ctx context.Context, stopped <-chan struct{}) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(ch)
	select {
	case sig := <-ch:
		log.Infof("Received %v signal: cleaning up...", sig)
	case <-stopped:
	case <-ctx.Done():
	}
}
