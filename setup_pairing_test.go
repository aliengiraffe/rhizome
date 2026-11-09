package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitGatewayReady(t *testing.T) {
	t.Run("ready after retries", func(t *testing.T) {
		admin := &fakeAdmin{failPeers: 3}
		if err := waitGatewayReady(context.Background(), "/run", admin.call, 5*time.Second); err != nil || len(admin.calls) != 4 {
			t.Fatal(err, admin.calls)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		admin := &fakeAdmin{failPeers: 1 << 20}
		start := time.Now()
		err := waitGatewayReady(context.Background(), "/run", admin.call, 300*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "gateway not ready") || !strings.Contains(err.Error(), "gateway service unavailable") || time.Since(start) > 3*time.Second {
			t.Fatal(err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		admin := &fakeAdmin{failPeers: 1}
		if err := waitGatewayReady(ctx, "/run", admin.call, time.Minute); err == nil {
			t.Fatal("ignored cancellation")
		}
	})
}

func TestNormalizeInvitation(t *testing.T) {
	for _, tc := range []struct{ in, want, err string }{
		{"rhz1_abc", "rhz1_abc", ""},
		{"  rhizome join rhz1_abc\n", "rhz1_abc", ""},
		{"", "", "empty"},
		{"rhizome join", "", "empty"},
		{"rhz1_a rhz1_b", "", "single code"},
		{strings.Repeat("x", maxInvitation+1), "", "single code"},
	} {
		got, err := normalizeInvitation(tc.in)
		if tc.err == "" && (err != nil || got != tc.want) {
			t.Fatalf("%q: %q %v", tc.in, got, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("%q: %v", tc.in, err)
		}
	}
}

func TestPairingOperations(t *testing.T) {
	admin := &fakeAdmin{}
	command, id, err := pairingInvite(context.Background(), "/run", admin.call)
	if err != nil || command != "rhizome join rhz1_secret" || id != "inv1" {
		t.Fatal(command, id, err)
	}
	lines := strings.Join(inviteLines(command, id), "\n")
	if !strings.Contains(lines, command) || !strings.Contains(lines, "no peer is connected yet") || !strings.Contains(lines, "revoke-invite inv1") {
		t.Fatal(lines)
	}
	admin.invite = errors.New("gateway service unavailable")
	if _, _, err := pairingInvite(context.Background(), "/run", admin.call); err == nil {
		t.Fatal("invite error swallowed")
	}
	j, err := pairingJoinHost(context.Background(), "/run", admin.call, "rhizome join rhz1_code")
	if err != nil || j.HostID != strings.Repeat("b", 64) || j.Connected {
		t.Fatal(j, err)
	}
	admin.connected = true
	if j, err = pairingJoinHost(context.Background(), "/run", admin.call, "rhz1_code"); err != nil || !j.Connected {
		t.Fatal(j, err)
	}
	if _, err := pairingJoinHost(context.Background(), "/run", admin.call, "rhz1_other"); err == nil {
		t.Fatal("rejected join reported as success")
	}
	if _, err := pairingJoinHost(context.Background(), "/run", admin.call, " "); err == nil || len(admin.calls) != 0 && admin.calls[len(admin.calls)-1] != "join" {
		t.Fatal("empty invitation reached the service", err, admin.calls)
	}
}

func TestOfferPairing(t *testing.T) {
	path := "/cfg/config.toml"
	for _, tc := range []struct {
		name, input string
		admin       *fakeAdmin
		wantErr     string
		wantOut     []string
		notOut      []string
	}{
		{"later by default", "\n", &fakeAdmin{}, "", []string{"Pair this device now?", "Pair later with the running service", "rhizome invite", "rhizome join <invitation>"}, []string{"rhz1_"}},
		{"invite", "1\n", &fakeAdmin{}, "", []string{"rhizome join rhz1_secret", "no peer is connected yet", "revoke-invite inv1"}, []string{"connected."}},
		{"invite failure", "invite\n", &fakeAdmin{invite: errors.New("gateway service unavailable")}, "invitation not created", []string{}, []string{"rhz1_"}},
		{"join connected", "2\nrhizome join rhz1_code\n", &fakeAdmin{connected: true}, "", []string{"Paired with host " + strings.Repeat("b", 64) + ": connected."}, nil},
		{"join pending", "join\nrhz1_code\n", &fakeAdmin{}, "", []string{"trust saved, connection not established yet"}, []string{": connected."}},
		{"join rejected", "2\nrhz1_code\n", &fakeAdmin{join: errors.New("enrollment rejected (HTTP 404); invitation may be expired, revoked or consumed")}, "join failed", []string{}, []string{"Paired"}},
		{"join invalid", "2\n\n", &fakeAdmin{}, "invitation is empty", nil, nil},
		{"bad choice retried", "x\n3\n", &fakeAdmin{}, "", []string{"Choose 1, 2 or 3", "Pair later"}, nil},
		{"input ends", "", &fakeAdmin{}, "input ended", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := offerPairing(context.Background(), prompter{in: bufioReader(tc.input), out: &out}, "/run", path, &out, tc.admin.call)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("%v\n%s", err, &out)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Fatalf("missing %q in\n%s", w, &out)
				}
			}
			for _, n := range tc.notOut {
				if strings.Contains(out.String(), n) {
					t.Fatalf("unexpected %q in\n%s", n, &out)
				}
			}
			if tc.wantErr != "" && tc.wantErr != "input ended" && !strings.Contains(err.Error(), "retry") {
				t.Fatalf("no retry path: %v", err)
			}
		})
	}
}

func TestPlainPairing(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	running := serviceOutcome{Installed: true, Running: true, Runtime: "/run"}
	for _, tc := range []struct {
		name, body, input string
		wizard            setupConfigResult
		outcome           serviceOutcome
		accepted          bool
		admin             *fakeAdmin
		wantOut           []string
		notOut            []string
		calls             int
	}{
		{"gateway off is silent", "[network]\nmode = 'local'\n", "", setupConfigResult{true, true}, running, true, &fakeAdmin{}, nil, []string{"Pair"}, 0},
		{"declined service", "[gateway]\nmode = 'lan'\n", "", setupConfigResult{true, true}, serviceOutcome{}, false, &fakeAdmin{}, []string{"Pair later: start the service first: rhizome service install --config", "rhizome invite"}, []string{"Pair this device now"}, 0},
		{"already running with new file", "[gateway]\nmode = 'lan'\n", "", setupConfigResult{true, true}, serviceOutcome{Installed: true, Running: true, WasRunning: true, Runtime: "/run"}, true, &fakeAdmin{}, []string{"rhizome service restart", "rhizome invite"}, []string{"Pair this device now"}, 0},
		{"already running with kept file offers pairing", "[gateway]\nmode = 'lan'\n", "3\n", setupConfigResult{true, false}, serviceOutcome{Installed: true, Running: true, WasRunning: true, Runtime: "/run"}, true, &fakeAdmin{}, []string{"Pair this device now", "Pair later with the running service"}, nil, 1},
		{"gateway not ready", "[gateway]\nmode = 'tailscale'\n", "", setupConfigResult{true, true}, running, true, &fakeAdmin{failPeers: 1 << 20}, []string{"gateway not ready", "rhizome service status", "rhizome join <invitation>"}, []string{"Pair this device now"}, 1},
		{"ready then invite", "[gateway]\nmode = 'lan'\n", "1\n", setupConfigResult{true, true}, running, true, &fakeAdmin{failPeers: 1}, []string{"Pair this device now", "rhizome join rhz1_secret", "no peer is connected yet"}, nil, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, tc.body)
			var out bytes.Buffer
			err := plainPairing(context.Background(), prompter{in: bufioReader(tc.input), out: &out}, path, tc.wizard, tc.outcome, tc.accepted, &out, tc.admin.call, 300*time.Millisecond)
			if err != nil {
				t.Fatalf("%v\n%s", err, &out)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Fatalf("missing %q in\n%s", w, &out)
				}
			}
			for _, n := range tc.notOut {
				if strings.Contains(out.String(), n) {
					t.Fatalf("unexpected %q in\n%s", n, &out)
				}
			}
			if tc.calls == 0 && len(tc.admin.calls) != 0 || tc.calls > 0 && len(tc.admin.calls) < tc.calls {
				t.Fatalf("admin calls %v", tc.admin.calls)
			}
			if b, _ := os.ReadFile(path); string(b) != tc.body {
				t.Fatal("pairing phase changed the configuration")
			}
		})
	}
}
