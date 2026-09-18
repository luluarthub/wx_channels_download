//go:build windows

package system

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDualProxyStateTransactionRestoresInconsistentSnapshots(t *testing.T) {
	legacyOn := windowsProxyLegacy{Enabled: 1, Server: "127.0.0.1:2023", EnablePresent: true, ServerPresent: true}
	legacyOff := legacyOn
	legacyOff.Enabled = 0
	for _, before := range []windowsProxyConnection{
		{Flags: 1, Server: "127.0.0.1:2023", Legacy: legacyOn},
		{Flags: 3, Server: "127.0.0.1:2023", Legacy: legacyOff},
		{Flags: 1, Server: "", Legacy: windowsProxyLegacy{}},
	} {
		for _, stage := range []string{"read connection", "read legacy", "set connection", "set legacy server", "set legacy enable", "legacy no effect", "notify", "read effective", "read IE", "IE mismatch", "success"} {
			t.Run(stage, func(t *testing.T) {
				current := before
				failure := errors.New("injected failure")
				failed := false
				fail := func(name string) error {
					if stage == name && !failed {
						failed = true
						return failure
					}
					return nil
				}
				store := windowsProxyStateStore{
					readConnection: func() (windowsProxyConnection, error) {
						return windowsProxyConnection{Flags: current.Flags, Server: current.Server}, fail("read connection")
					},
					writeConnection: func(value windowsProxyConnection) error {
						current.Flags, current.Server = value.Flags, value.Server
						return fail("set connection")
					},
					readLegacy: func() (windowsProxyLegacy, error) { return current.Legacy, fail("read legacy") },
					writeLegacy: func(value windowsProxyLegacy) error {
						current.Legacy.Server, current.Legacy.ServerPresent = value.Server, value.ServerPresent
						if err := fail("set legacy server"); err != nil {
							return err
						}
						if fail("legacy no effect") != nil {
							current.Legacy.Enabled = 0
							return nil
						}
						current.Legacy.Enabled, current.Legacy.EnablePresent = value.Enabled, value.EnablePresent
						return fail("set legacy enable")
					},
					readEffective: func() (windowsProxyConnection, error) { return current, fail("read effective") },
					readIEProxy: func() (string, error) {
						if fail("IE mismatch") != nil {
							return "unexpected-proxy", nil
						}
						if err := fail("read IE"); err != nil {
							return "", err
						}
						if current.Legacy.EnablePresent && current.Legacy.Enabled != 0 {
							return current.Legacy.Server, nil
						}
						return "", nil
					},
				}
				api := windowsProxyConnectionAPI{read: store.read, write: store.write, restore: store.restore, verify: store.verify, notify: func() error { return fail("notify") }}
				ownerPath := filepath.Join(t.TempDir(), "owner")
				if err := writeProxyOwner(ownerPath, "prior"); err != nil {
					t.Fatal(err)
				}
				err := configureWindowsProxy(ProxySettings{Hostname: "127.0.0.1", Port: "21203"}, ownerPath, "current", api)
				owner, _ := os.ReadFile(ownerPath)
				if stage == "success" {
					if err != nil || current.Flags&2 == 0 || current.Server != "127.0.0.1:21203" || current.Legacy.Enabled != 1 || current.Legacy.Server != current.Server || string(owner) != "current" {
						t.Fatalf("dual-state commit failed: %+v owner=%s err=%v", current, owner, err)
					}
				} else if err == nil || current != before || string(owner) != "prior" {
					t.Fatalf("dual-state rollback failed: before=%+v after=%+v owner=%s err=%v", before, current, owner, err)
				}
			})
		}
	}
}

func TestWindowsProxyStatusRejectsInconsistentViews(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		state                  windowsProxyConnection
		wantEnabled, wantError bool
	}{
		{"both disabled", windowsProxyConnection{Flags: 1}, false, false},
		{"native enabled legacy disabled", windowsProxyConnection{Flags: 3, Server: "127.0.0.1:2023"}, false, true},
		{"native disabled legacy enabled", windowsProxyConnection{Flags: 1, Legacy: windowsProxyLegacy{EnablePresent: true, Enabled: 1, Server: "127.0.0.1:2023"}}, false, true},
		{"different active addresses", windowsProxyConnection{Flags: 3, Server: "127.0.0.1:2023", Legacy: windowsProxyLegacy{EnablePresent: true, Enabled: 1, Server: "127.0.0.1:7897"}}, false, true},
		{"both enabled", windowsProxyConnection{Flags: 3, Server: "127.0.0.1:2023", Legacy: windowsProxyLegacy{EnablePresent: true, Enabled: 1, Server: "127.0.0.1:2023"}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, err := proxySettingsFromWindowsState(tc.state)
			if (err != nil) != tc.wantError || (settings != nil) != tc.wantEnabled {
				t.Fatalf("status settings=%+v err=%v", settings, err)
			}
		})
	}
}

func TestDualProxyRestoreAttemptsBothViews(t *testing.T) {
	connectionErr, legacyErr := errors.New("connection restore failed"), errors.New("legacy restore failed")
	legacyAttempted := false
	store := windowsProxyStateStore{
		writeConnection: func(windowsProxyConnection) error { return connectionErr },
		writeLegacy:     func(windowsProxyLegacy) error { legacyAttempted = true; return legacyErr },
	}
	err := store.restore(windowsProxyConnection{})
	if !legacyAttempted || !errors.Is(err, connectionErr) || !errors.Is(err, legacyErr) {
		t.Fatalf("restore skipped a view: %v", err)
	}
}

func TestDualProxyCleanupProtectsEveryEnabledView(t *testing.T) {
	expected := ProxySettings{Hostname: "127.0.0.1", Port: "2023"}
	for _, tc := range []struct {
		name  string
		state windowsProxyConnection
		want  bool
	}{
		{"legacy only owned", windowsProxyConnection{Flags: 1, Server: "other:99", Legacy: windowsProxyLegacy{Enabled: 1, Server: "127.0.0.1:2023", EnablePresent: true}}, true},
		{"native only owned", windowsProxyConnection{Flags: 3, Server: "127.0.0.1:2023", Legacy: windowsProxyLegacy{Server: "other:99"}}, true},
		{"legacy foreign", windowsProxyConnection{Flags: 3, Server: "127.0.0.1:2023", Legacy: windowsProxyLegacy{Enabled: 1, Server: "other:99", EnablePresent: true}}, false},
		{"native foreign", windowsProxyConnection{Flags: 3, Server: "other:99", Legacy: windowsProxyLegacy{Enabled: 1, Server: "127.0.0.1:2023", EnablePresent: true}}, false},
		{"legacy mixed", windowsProxyConnection{Flags: 1, Legacy: windowsProxyLegacy{Enabled: 1, Server: "http=127.0.0.1:2023;https=other:99", EnablePresent: true}}, false},
		{"unknown active native", windowsProxyConnection{Flags: 3, Legacy: windowsProxyLegacy{Enabled: 1, Server: "127.0.0.1:2023", EnablePresent: true}}, false},
		{"both disabled", windowsProxyConnection{Flags: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := tc.state
			writes := 0
			api := windowsProxyConnectionAPI{
				read:   func() (windowsProxyConnection, error) { return current, nil },
				write:  func(state windowsProxyConnection) error { writes++; current = state; return nil },
				notify: func() error { return nil },
			}
			changed, err := disableWindowsProxy(api, &expected)
			if err != nil || changed != tc.want || (writes != 0) != tc.want {
				t.Fatalf("cleanup changed=%v writes=%d err=%v", changed, writes, err)
			}
			if tc.want && (current.Flags&2 != 0 || current.Legacy.Enabled != 0 || current.Server != tc.state.Server || current.Legacy.Server != tc.state.Legacy.Server) {
				t.Fatalf("cleanup changed servers or left a view enabled: %+v", current)
			}
		})
	}
}
