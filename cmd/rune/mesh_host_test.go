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
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"unstable.build/rune/auth"
	"unstable.build/rune/cmd/rune/ide/apiclient"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/runenet"
	"unstable.build/rune/internal/runenet/meshrpc"
)

// watchHost serves host's mesh service in memory and returns a client
// subscribed to its changes.
func watchHost(t *testing.T, host *meshHost) <-chan meshrpc.Change {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	host.server.Register(grpcServer)
	go debug.CapturePanicReport(func() { _ = grpcServer.Serve(lis) })
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///mesh",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, err := meshrpc.NewClient(conn).Watch(ctx)
	require.NoError(t, err)
	return events
}

func nextChange(t *testing.T, events <-chan meshrpc.Change) meshrpc.Change {
	t.Helper()
	select {
	case ev, ok := <-events:
		require.True(t, ok, "watch ended")
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no change reported")
		return meshrpc.Change{}
	}
}

// A logout is either a node key that reached its lifetime, which is
// re-minted silently, or a machine the account removed, which the
// editors are told about through the Watch stream. Only the account's
// machine list tells them apart, so an unreachable account server
// claims neither. The daemon never prompts: it has no user in front of
// it.
func TestMeshHostHandleNeedsLogin(t *testing.T) {
	setup := func(t *testing.T, api *stubNetworkAPI) (*meshHost, <-chan meshrpc.Change) {
		t.Helper()
		host := newMeshHost(config.NopConfig(), t.TempDir(), newNetworkGate(api), nil)
		t.Cleanup(func() { _ = host.Close() })
		events := watchHost(t, host)
		assert.False(t, nextChange(t, events).MachineRemoved)
		return host, events
	}

	t.Run("reports a machine the account no longer lists", func(t *testing.T) {
		api := &stubNetworkAPI{
			user:     auth.RPCUser{Role: auth.RoleUser},
			signedIn: true,
			machines: []apiclient.Machine{{ID: "1", Hostname: "laptop"}},
		}
		host, events := setup(t, api)
		host.handleNeedsLogin(context.Background(), "9")
		assert.True(t, host.removed.Load())
		assert.True(t, nextChange(t, events).MachineRemoved)
		assert.Zero(t, api.credCalls.Load())
	})

	t.Run("re-registers silently when it is still listed", func(t *testing.T) {
		api := &stubNetworkAPI{
			user:     auth.RPCUser{Role: auth.RoleUser},
			signedIn: true,
			machines: []apiclient.Machine{{ID: "1", Hostname: "laptop"}},
			credErr:  errors.New("credentials endpoint unreachable"),
		}
		host, _ := setup(t, api)
		host.handleNeedsLogin(context.Background(), "1")
		assert.False(t, host.removed.Load())
		assert.Equal(t, int32(1), api.credCalls.Load())
	})

	t.Run("says nothing when the account cannot be reached", func(t *testing.T) {
		api := &stubNetworkAPI{
			user:       auth.RPCUser{Role: auth.RoleUser},
			signedIn:   true,
			machineErr: errors.New("account server unreachable"),
		}
		host, _ := setup(t, api)
		host.handleNeedsLogin(context.Background(), "1")
		assert.False(t, host.removed.Load())
		assert.Zero(t, api.credCalls.Load())
	})
}

// The editor's login and logout write the token to the shared data
// directory; the daemon only sees them if it reads the token again
// before every mint.
func TestMeshHostReloadsTokenBeforeMinting(t *testing.T) {
	api := &stubNetworkAPI{
		user:       auth.RPCUser{Role: auth.RoleUser},
		signedIn:   true,
		controlURL: "https://control.example.com",
		authKey:    "tskey-1",
	}
	reloads := 0
	host := newMeshHost(config.NopConfig(), t.TempDir(), newNetworkGate(api),
		func() { reloads++ })
	t.Cleanup(func() { _ = host.Close() })

	controlURL, authKey, err := host.credentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "https://control.example.com", controlURL)
	assert.Equal(t, "tskey-1", authKey)
	assert.Equal(t, 1, reloads)
	assert.Equal(t, int32(1), api.credCalls.Load())
}

// A spawned daemon joins at start when the user asked for it and
// never prompts: a signed-out account is a log line, and the join is
// still attempted so the token the editor stores later is picked up
// by the next one.
func TestMeshHostStartAutoJoin(t *testing.T) {
	for _, tc := range []struct {
		name      string
		autoJoin  bool
		wantCreds int
	}{
		{"auto_join on asks for credentials", true, 1},
		{"auto_join off does not", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &stubNetworkAPI{
				user:     auth.RPCUser{Role: auth.RoleUser},
				signedIn: true,
				credErr:  runenet.ErrSubscriptionRequired,
			}
			cfg := config.MapConfig(map[string]any{
				"network": map[string]any{"auto_join": tc.autoJoin},
			})
			host := newMeshHost(cfg, t.TempDir(), newNetworkGate(api), nil)
			t.Cleanup(func() { _ = host.Close() })

			host.startAutoJoin()
			if tc.wantCreds == 0 {
				// Disabled, it returns without starting a join at all.
				assert.Zero(t, api.credCalls.Load())
				return
			}
			assert.Eventually(t, func() bool {
				return int(api.credCalls.Load()) == tc.wantCreds
			}, 5*time.Second, 10*time.Millisecond)
			st, err := host.Status(context.Background())
			require.NoError(t, err)
			assert.Equal(t, runenet.StateNotRunning, st.State)
			assert.Equal(t, runenet.ErrSubscriptionRequired.Error(), st.LastError)
		})
	}
}
