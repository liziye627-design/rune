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

package workspacedtest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"unstable.build/rune/internal/workspace/vtescheme"
	"unstable.build/rune/internal/workspace/workspacessh"
	sshtest "unstable.build/rune/internal/workspace/workspacessh/test"
)

// home is the ssh user's home directory on a container.
const home = "/home/test"

// container is a machine a Rune reaches over ssh, running the real rune
// as the ssh user.
type container struct {
	id       string
	hostPort string
}

func startContainer(t *testing.T) *container {
	t.Helper()
	skipWithoutDocker(t)
	bin := linuxRune.get(t)
	image, err := ensureImage("rune_e2e_machine", "Dockerfile",
		filepath.Join("internal", "workspace"))
	require.NoError(t, err)

	args := []string{"run", "-d", "--rm", "-P", "-v", bin + ":/opt/rune/rune:ro"}
	if dir := os.Getenv(coverDirEnv); dir != "" {
		args = append(args, "-v", dir+":/tmp/cover")
	}
	out, err := exec.Command("docker", append(args, image)...).CombinedOutput()
	require.NoError(t, err, "docker run: %s", out)
	c := &container{id: strings.TrimSpace(string(out))}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", c.id).Run() })
	// Cleanups run last to first: the machine is inspected before it
	// goes away.
	t.Cleanup(func() { c.checkRaces(t) })
	t.Cleanup(c.stopRunes)

	ports, err := exec.Command("docker", "port", c.id, "22/tcp").Output()
	require.NoError(t, err)
	first, _, _ := strings.Cut(strings.TrimSpace(string(ports)), "\n")
	_, port, err := net.SplitHostPort(first)
	require.NoError(t, err, "docker port %q", ports)
	c.hostPort = net.JoinHostPort("127.0.0.1", port)
	eventually(t, "sshd to accept connections", func() (bool, string) {
		err := sshBanner(c.hostPort)
		return err == nil, fmt.Sprint(err)
	})
	return c
}

func sshBanner(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	banner := make([]byte, 4)
	if _, err := conn.Read(banner); err != nil {
		return err
	}
	if string(banner) != "SSH-" {
		return fmt.Errorf("unexpected banner %q", banner)
	}
	return nil
}

// run runs script as the ssh user, reporting its output and exit code.
func (c *container) run(t *testing.T, script string) (string, int) {
	t.Helper()
	out, err := exec.Command("docker", "exec", "-u", "test", "-w", home,
		"-e", "GORACE=log_path=/tmp/race/race", "-e", "GOCOVERDIR=/tmp/cover",
		c.id, "sh", "-c", script).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	require.NoError(t, err, "docker exec: %s", out)
	return string(out), 0
}

// sh runs script as the ssh user, which must succeed.
func (c *container) sh(t *testing.T, script string) string {
	t.Helper()
	out, code := c.run(t, script)
	require.Zero(t, code, "%s: %s", script, out)
	return out
}

// runes lists the rune processes on the machine by pid.
func (c *container) runes(t *testing.T) map[int]string {
	t.Helper()
	ret := liveProcesses(c.sh(t, "ps -eo pid=,stat=,args="))
	maps.DeleteFunc(ret, func(_ int, args string) bool {
		return filepath.Base(strings.Fields(args)[0]) != "rune"
	})
	return ret
}

// servers lists the machine's headless runes by pid.
func (c *container) servers(t *testing.T) map[int]string {
	t.Helper()
	ret := make(map[int]string)
	for pid, args := range c.runes(t) {
		if strings.Contains(args, "--headless") {
			ret[pid] = args
		}
	}
	return ret
}

// serverPID is the process holding the lock of the default data
// directory.
func (c *container) serverPID(t *testing.T) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(c.sh(t, "cat .rune/servers/rune.lock")))
	require.NoError(t, err)
	return pid
}

// client is a Rune reaching the workspace at dir over ssh.
func (c *container) client(t *testing.T, dir string) vtescheme.Scheme {
	t.Helper()
	c.sh(t, "mkdir -p "+dir)
	cfg := config.MapConfig(map[string]any{
		"private_keys": []any{sshtest.PrivateKeyPath(t, "id_ed25519")},
		"timeout":      "20s",
		"insecure":     true,
	})
	uri, err := workspaceapi.ParseURI("ssh://test@" + c.hostPort + dir)
	require.NoError(t, err)
	s, err := workspacessh.New(errorUI{})(context.Background(), cfg, uri)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	owner, ok := s.(vtescheme.Scheme)
	require.True(t, ok, "an ssh workspace owns terminals")
	return owner
}

// stopRunes asks every rune on the machine to exit, the way the machine
// shutting down would. Only a rune that exits on its own reports its
// coverage and its races.
func (c *container) stopRunes() {
	script := fmt.Sprintf("pkill -TERM -x rune; for i in $(seq %d); do "+
		"pgrep -x rune >/dev/null || exit 0; sleep 0.1; done",
		int(stopTimeout/(100*time.Millisecond)))
	_ = exec.Command("docker", "exec", c.id, "sh", "-c", script).Run()
}

// checkRaces fails the test on any data race a rune on the machine
// reported.
func (c *container) checkRaces(t *testing.T) {
	out, _ := exec.Command("docker", "exec", c.id, "sh", "-c",
		"cat /tmp/race/race.* 2>/dev/null").Output()
	if len(out) > 0 {
		t.Errorf("rune reported data races on the machine:\n%s", out)
	}
}

// errorUI fails every prompt: the tests authenticate with a key, so a
// prompt means the scenario is not what it claims to be.
type errorUI struct{}

func (errorUI) PromptSecret(context.Context, string) (string, error) {
	return "", errors.New("unexpected secret prompt")
}

func (errorUI) PromptText(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected text prompt")
}

func (errorUI) PromptChoice(context.Context, string, []string) (int, error) {
	return 0, errors.New("unexpected choice prompt")
}

func (errorUI) Notify(workspacessh.NotificationLevel, string) {}
