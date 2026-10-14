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

//go:build e2e

// Package workspacedtest holds the end-to-end tests of a machine's
// workspace server the way users run it: the real rune binary, started
// by an operator as `rune --headless` or on demand by a Rune that needs
// its terminals, and reached directly, through `rune --vte-bridge` and
// over ssh.
//
// Every rune these tests start is built with the race detector, and a
// race any of them reports fails the run: a server that outlives its
// clients has nobody else to report it to.
package workspacedtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/term/vte"
	"unstable.build/rune/internal/workspace"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspacetest"
)

// eventuallyTimeout bounds every wait below. A race-enabled server, and
// on the ssh paths a reconnect with its backoff, make it generous.
const eventuallyTimeout = 60 * time.Second

const eventuallyInterval = 50 * time.Millisecond

// stopTimeout is how long a rune asked to exit gets before it is
// killed. Only a rune that exits on its own reports its coverage.
const stopTimeout = 10 * time.Second

// coverDirEnv, when set, builds the host rune with coverage and collects
// the coverage of every rune the tests start there, including the
// servers they start in turn.
const coverDirEnv = "RUNE_E2E_GOCOVERDIR"

func TestMain(m *testing.M) {
	if os.Getenv(viewerEnv) != "" {
		// A viewer reports its races where the test that started it
		// collects them.
		os.Exit(m.Run())
	}
	raceDir, err := os.MkdirTemp("", "rune-e2e-race")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Every rune started here, and every server those start in turn,
	// inherits the environment: it is the only way to hear from a
	// server whose standard error nobody reads.
	_ = os.Setenv("GORACE", "log_path="+filepath.Join(raceDir, "race"))
	if dir := os.Getenv(coverDirEnv); dir != "" {
		_ = os.Setenv("GOCOVERDIR", dir)
	}

	code := m.Run()
	if reports := readRaceReports(raceDir); reports != "" {
		fmt.Fprintf(os.Stderr, "rune reported data races:\n%s", reports)
		code = 1
	}
	_ = os.RemoveAll(raceDir)
	os.Exit(code)
}

func readRaceReports(dir string) string {
	paths, _ := filepath.Glob(filepath.Join(dir, "race.*"))
	var b strings.Builder
	for _, path := range paths {
		report, err := os.ReadFile(path)
		if err == nil {
			fmt.Fprintf(&b, "%s:\n%s\n", path, report)
		}
	}
	return b.String()
}

// artifact is something built once per test binary.
type artifact struct {
	once  sync.Once
	build func() (string, error)
	path  string
	err   error
}

func (a *artifact) get(t *testing.T) string {
	t.Helper()
	a.once.Do(func() { a.path, a.err = a.build() })
	if a.err != nil {
		t.Fatal(a.err)
	}
	return a.path
}

// hostRune is cmd/rune for the machine running the tests.
var hostRune = &artifact{build: func() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	out := filepath.Join(root, "target", "e2e",
		fmt.Sprintf("rune-%s-%s", runtime.GOOS, runtime.GOARCH))
	args := []string{"build", "-race", "-tags=ebitensinglethread", "-o", out}
	if os.Getenv(coverDirEnv) != "" {
		args = append(args, "-cover", "-coverpkg=unstable.build/rune/...")
	}
	cmd := exec.Command("go", append(args, "./cmd/rune")...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build rune: %w: %s", err, b)
	}
	return out, nil
}}

// linuxRune is cmd/rune for the containers, built in a container: the
// GUI's headers it compiles against only exist on Linux.
var linuxRune = &artifact{build: func() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	image, err := ensureImage("rune_e2e_builder", "Dockerfile.build", "")
	if err != nil {
		return "", err
	}
	arch := containerArch()
	outDir := filepath.Join(root, "target", "e2e")
	cache := filepath.Join(outDir, "gocache-linux-"+arch)
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	modCache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMODCACHE: %w", err)
	}
	name := "rune-linux-" + arch
	build := []string{"go", "build", "-buildvcs=false", "-race",
		"-tags=ebitensinglethread", "-o", "/out/" + name}
	if os.Getenv(coverDirEnv) != "" {
		build = append(build, "-cover", "-coverpkg=unstable.build/rune/...")
	}
	// The host's module cache already holds every module, including the
	// private ones, so the build needs neither network nor credentials.
	// Running as the host user keeps the caches it writes the host's,
	// and a worktree's .git names a path that only exists on the host.
	cmd := exec.Command("docker", append([]string{"run", "--rm",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "HOME=/tmp",
		"-e", "GOCACHE=/gocache",
		"-e", "GOMODCACHE=/gomod",
		"-e", "GOPROXY=off",
		"-e", "GOFLAGS=-mod=readonly",
		"-e", "GOTOOLCHAIN=local",
		"-e", "CGO_ENABLED=1",
		"-v", root + ":/src:ro",
		"-v", strings.TrimSpace(string(modCache)) + ":/gomod:ro",
		"-v", cache + ":/gocache",
		"-v", outDir + ":/out",
		"-w", "/src",
		image,
	}, append(build, "./cmd/rune")...)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build rune for linux: %w: %s", err, b)
	}
	return filepath.Join(outDir, name), nil
}}

// ensureImage builds dockerfile, from this directory, as name unless an
// image of the same contents exists. contextDir, relative to the
// repository, defaults to an empty context.
func ensureImage(name, dockerfile, contextDir string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "internal", "workspace", "workspaced", "test", dockerfile)
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	tag := name + ":" + hex.EncodeToString(sum[:])[:12]
	if exec.Command("docker", "image", "inspect", tag).Run() == nil {
		return tag, nil
	}

	buildCtx := filepath.Join(root, contextDir)
	if contextDir == "" {
		if buildCtx, err = os.MkdirTemp("", "rune-e2e-ctx"); err != nil {
			return "", err
		}
		defer os.RemoveAll(buildCtx)
	}
	cmd := exec.Command("docker", "build", "-t", tag, "-f", path, buildCtx)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker build %s: %w: %s", tag, err, b)
	}
	return tag, nil
}

// containerArch runs the containers natively: the repository's Go
// toolchain misbehaves under QEMU emulation.
func containerArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

var repoRoot = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out))), nil
})

func skipWithoutDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
}

// process is a program a test started, with everything it printed.
type process struct {
	cmd    *exec.Cmd
	stdout lockedBuffer
	stderr lockedBuffer
	done   chan struct{}
}

func startProcess(t *testing.T, name string, args ...string) *process {
	t.Helper()
	p := &process{cmd: exec.Command(name, args...), done: make(chan struct{})}
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	require.NoError(t, p.cmd.Start())
	go debug.CapturePanicReport(func() {
		_ = p.cmd.Wait()
		close(p.done)
	})
	t.Cleanup(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(stopTimeout):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// exitCode waits for the process to exit on its own.
func (p *process) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(eventuallyTimeout):
		t.Fatalf("%s did not exit\n%s", p.cmd, p.output())
		return -1
	}
}

func (p *process) output() string {
	return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", p.stdout.String(), p.stderr.String())
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// eventually polls cond until it holds, reporting the last thing it saw
// when it never does.
//
// Unlike require.Eventually, cond runs on the test goroutine, so it
// may fail t through require.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(eventuallyTimeout)
	var last string
	for time.Now().Before(deadline) {
		ok, seen := cond()
		if ok {
			return
		}
		last = seen
		time.Sleep(eventuallyInterval)
	}
	t.Fatalf("timed out waiting for %s; last saw: %s", what, last)
}

// screen renders what a terminal shows as text.
func screen(v workspace.VTE) string {
	snap, err := v.Snapshot()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, row := range snap.ActiveCells() {
		for _, cell := range row {
			if cell.Ch == 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(cell.Ch)
		}
		b.WriteRune('\n')
	}
	return b.String()
}

func awaitScreen(t *testing.T, v workspace.VTE, want string) {
	t.Helper()
	eventually(t, fmt.Sprintf("the terminal to show %q", want), func() (bool, string) {
		got := screen(v)
		return strings.Contains(got, want), fmt.Sprintf("%q", got)
	})
}

// typeLine feeds a line to a terminal the way a user would.
func typeLine(v workspace.VTE, line string) {
	for _, r := range line {
		v.Handle(term.Event{Type: term.EventKey, Ch: r, Raw: []byte(string(r))})
	}
	v.Handle(term.Event{Type: term.EventKey, Key: term.KeyEnter, Raw: []byte{'\r'}})
}

// terminalOptions runs argv in a terminal big enough for any marker.
func terminalOptions(argv ...string) workspace.CreateVTEOptions {
	return workspace.CreateVTEOptions{
		CommandAndArgs: argv,
		Width:          120,
		Height:         24,
		MaxLines:       1000,
	}
}

// vteCtx carries what a terminal replays its stream into: a test has no
// browser, and only looks at the grid.
func vteCtx() context.Context {
	return vtescheme.WithClientVTEEnv(context.Background(), vtescheme.ClientVTEEnv{
		Publisher:     workspacetest.NopVTEHost{},
		Notifications: workspacetest.NopVTEHost{},
		Tabs:          workspacetest.NopVTEHost{},
		Config:        vte.DefaultConfig(),
	})
}
