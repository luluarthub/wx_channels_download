//go:build windows

package system

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnableProxyRollsBackEveryFailedStage(t *testing.T) {
	for _, failureAt := range []string{"read", "server", "enable", "notify", "success"} {
		t.Run(failureAt, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxy-owner")
			if err := writeProxyOwner(path, "previous-owner"); err != nil {
				t.Fatal(err)
			}
			values := map[string]string{"ProxyServer": "127.0.0.1:7897", "ProxyEnable": "0x1"}
			failure := errors.New("injected registry failure")
			failed := false
			fail := func(stage string) error {
				if failureAt == stage && !failed {
					failed = true
					return failure
				}
				return nil
			}
			registry := windowsProxyRegistry{
				read: func(name string) (string, error) { return values[name], fail("read") },
				write: func(name, value string) error {
					// Even an operation that mutates before returning an error is rolled back.
					values[name] = value
					if name == "ProxyServer" {
						return fail("server")
					}
					return fail("enable")
				},
				remove: func(name string) error { delete(values, name); return nil },
				notify: func() error { return fail("notify") },
			}
			err := configureWindowsProxy(ProxySettings{Hostname: "127.0.0.1", Port: "2023"}, path, "new-owner", registry)
			owner, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if failureAt == "success" {
				if err != nil || string(owner) != "new-owner" || values["ProxyServer"] != "127.0.0.1:2023" || values["ProxyEnable"] != "1" {
					t.Fatalf("failed commit: %v %s %v", err, owner, values)
				}
			} else if !errors.Is(err, failure) || string(owner) != "previous-owner" || values["ProxyServer"] != "127.0.0.1:7897" || values["ProxyEnable"] != "0x1" {
				t.Fatalf("failed rollback: %v %s %v", err, owner, values)
			}
		})
	}
}

func TestEnableProxyOwnerWriteFailureDoesNotTouchRegistry(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := windowsProxyRegistry{
		read:   func(string) (string, error) { return "", nil },
		write:  func(string, string) error { t.Fatal("must not write registry"); return nil },
		notify: func() error { t.Fatal("must not notify"); return nil },
	}
	if err := configureWindowsProxy(ProxySettings{}, filepath.Join(blocked, "proxy-owner"), "new", registry); err == nil {
		t.Fatal("expected owner write failure")
	}
}

func TestEnableProxyRollbackRestoresAbsentValuesAndOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-owner")
	values := map[string]string{}
	failed := false
	registry := windowsProxyRegistry{
		read:   func(name string) (string, error) { return values[name], nil },
		write:  func(name, value string) error { values[name] = value; return nil },
		remove: func(name string) error { delete(values, name); return nil },
		notify: func() error {
			if !failed {
				failed = true
				return errors.New("notify failed")
			}
			return nil
		},
	}
	if err := configureWindowsProxy(ProxySettings{Hostname: "127.0.0.1", Port: "2023"}, path, "new", registry); err == nil {
		t.Fatal("expected notify failure")
	}
	if len(values) != 0 {
		t.Fatalf("registry values not removed: %v", values)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("owner not removed: %v", err)
	}
}
