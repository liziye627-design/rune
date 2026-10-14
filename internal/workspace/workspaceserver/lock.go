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

package workspaceserver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrLocked is returned by [AcquireLock] when another workspace server
// already holds the lock of the data directory.
var ErrLocked = errors.New("there's another instance of Rune holding the lock")

// LockPath returns the lock guarding the workspace server of dataDir.
func LockPath(dataDir string) string {
	return filepath.Join(SocketDir(dataDir), "rune.lock")
}

// Lock is the exclusive right to serve the workspaces of a data
// directory.
type Lock struct {
	f *os.File
}

// AcquireLock takes the lock of dataDir without waiting for it.
//
// The lock is an advisory flock rather than the socket: the kernel
// releases it the moment its holder dies, however it dies, so a crashed
// server never locks out its successor, and taking it is atomic, so two
// servers starting at once cannot both win.
func AcquireLock(dataDir string) (*Lock, error) {
	path := LockPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, socketPerm)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		holder := readHolder(f)
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, lockedError(path, holder)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// The holder's pid is only there so whoever loses the race can say
	// who won; the flock alone is the lock.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

// Release gives the lock up. The file stays behind: removing it would
// let a starter lock a fresh inode while another still waits on this
// one, and two servers would each believe they hold the lock.
func (l *Lock) Release() error {
	return l.f.Close()
}

func readHolder(f *os.File) int {
	b, err := io.ReadAll(io.NewSectionReader(f, 0, 32))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

func lockedError(path string, holder int) error {
	if holder <= 0 {
		return fmt.Errorf("%w on %s", ErrLocked, path)
	}
	return fmt.Errorf("%w on %s (pid %d)", ErrLocked, path, holder)
}
