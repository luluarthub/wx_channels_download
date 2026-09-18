//go:build windows

package system

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestEnableProxyRollsBackEveryFailedStage(t *testing.T) {
	for _, failureAt := range []string{"read", "set", "notify", "success"} {
		t.Run(failureAt, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxy-owner")
			if err := writeProxyOwner(path, "previous-owner"); err != nil {
				t.Fatal(err)
			}
			previous := windowsProxyConnection{Flags: 1 | 2 | 4 | 8, Server: "127.0.0.1:7897"}
			current := previous
			failure := errors.New("injected connection failure")
			failed := false
			fail := func(stage string) error {
				if failureAt == stage && !failed {
					failed = true
					return failure
				}
				return nil
			}
			connection := windowsProxyConnectionAPI{
				read: func() (windowsProxyConnection, error) { return current, fail("read") },
				write: func(value windowsProxyConnection) error {
					// WinINET may partially apply an option list before an error.
					current = value
					return fail("set")
				},
				notify: func() error { return fail("notify") },
			}
			err := configureWindowsProxy(ProxySettings{Hostname: "127.0.0.1", Port: "2023"}, path, "new-owner", connection)
			owner, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if failureAt == "success" {
				want := windowsProxyConnection{Flags: previous.Flags, Server: "127.0.0.1:2023"}
				if err != nil || string(owner) != "new-owner" || current != want {
					t.Fatalf("failed commit: %v %s %+v", err, owner, current)
				}
			} else if !errors.Is(err, failure) || string(owner) != "previous-owner" || current != previous {
				t.Fatalf("failed rollback: %v %s %+v", err, owner, current)
			}
		})
	}
}

func TestEnableProxyOwnerWriteFailureDoesNotChangeConnection(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	connection := windowsProxyConnectionAPI{
		read:   func() (windowsProxyConnection, error) { return windowsProxyConnection{}, nil },
		write:  func(windowsProxyConnection) error { t.Fatal("must not set proxy"); return nil },
		notify: func() error { t.Fatal("must not notify"); return nil },
	}
	if err := configureWindowsProxy(ProxySettings{}, filepath.Join(blocked, "proxy-owner"), "new", connection); err == nil {
		t.Fatal("expected owner write failure")
	}
}

func TestEnableProxySetsManualConnectionFlagEvenWhenServerAlreadyMatches(t *testing.T) {
	// Regression: connection flags remained DIRECT while ProxyEnable was 1.
	for _, flags := range []uint32{1, 1 | 4, 1 | 8, 1 | 4 | 8} {
		current := windowsProxyConnection{Flags: flags, Server: "127.0.0.1:2023"}
		writes := 0
		connection := windowsProxyConnectionAPI{
			read:   func() (windowsProxyConnection, error) { return current, nil },
			write:  func(value windowsProxyConnection) error { current = value; writes++; return nil },
			notify: func() error { return nil },
		}
		if err := configureWindowsProxy(ProxySettings{Hostname: "127.0.0.1", Port: "2023"}, filepath.Join(t.TempDir(), "owner"), "new", connection); err != nil {
			t.Fatal(err)
		}
		if current.Flags != flags|proxyTypeProxy || writes != 1 {
			t.Fatalf("manual flag not synchronized or auto settings changed: %+v writes=%d", current, writes)
		}
	}
}

func TestEnableProxyRollbackRestoresAbsentOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-owner")
	previous := windowsProxyConnection{Flags: proxyTypeDirect}
	current := previous
	failed := false
	connection := windowsProxyConnectionAPI{
		read:  func() (windowsProxyConnection, error) { return current, nil },
		write: func(value windowsProxyConnection) error { current = value; return nil },
		notify: func() error {
			if !failed {
				failed = true
				return errors.New("notify failed")
			}
			return nil
		},
	}
	if err := configureWindowsProxy(ProxySettings{Hostname: "127.0.0.1", Port: "2023"}, path, "new", connection); err == nil {
		t.Fatal("expected notify failure")
	}
	if current != previous {
		t.Fatalf("connection not restored: %+v", current)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("owner not removed: %v", err)
	}
}

func TestEnableProxyRollbackFailureKeepsGuardianOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner")
	if err := writeProxyOwner(path, "previous"); err != nil {
		t.Fatal(err)
	}
	setFailure := errors.New("set failed after mutation")
	restoreFailure := errors.New("restore failed")
	writes := 0
	current := windowsProxyConnection{Flags: 1, Server: "127.0.0.1:7897"}
	connection := windowsProxyConnectionAPI{
		read: func() (windowsProxyConnection, error) { return current, nil },
		write: func(value windowsProxyConnection) error {
			writes++
			if writes == 1 {
				current = value
				return setFailure
			}
			return restoreFailure
		},
		notify: func() error { return nil },
	}
	err := configureWindowsProxy(ProxySettings{Hostname: "127.0.0.1", Port: "2023"}, path, "new", connection)
	owner, _ := os.ReadFile(path)
	if !errors.Is(err, setFailure) || !errors.Is(err, restoreFailure) || string(owner) != "new" || current.Server != "127.0.0.1:2023" {
		t.Fatalf("lost recoverable ownership: err=%v owner=%s state=%+v", err, owner, current)
	}
}

func TestDisableProxyPreservesOtherConnectionSettingsAndRollsBack(t *testing.T) {
	for _, failureAt := range []string{"read", "set", "notify", "success"} {
		t.Run(failureAt, func(t *testing.T) {
			previous := windowsProxyConnection{Flags: 1 | 2 | 4 | 8, Server: "127.0.0.1:2023"}
			current := previous
			failure := errors.New("disable failed")
			failed := false
			fail := func(stage string) error {
				if failureAt == stage && !failed {
					failed = true
					return failure
				}
				return nil
			}
			connection := windowsProxyConnectionAPI{
				read:   func() (windowsProxyConnection, error) { return current, fail("read") },
				write:  func(value windowsProxyConnection) error { current = value; return fail("set") },
				notify: func() error { return fail("notify") },
			}
			expected := ProxySettings{Hostname: "127.0.0.1", Port: "2023"}
			changed, err := disableWindowsProxy(connection, &expected)
			if failureAt == "success" {
				want := previous
				want.Flags &^= proxyTypeProxy
				if err != nil || !changed || current != want {
					t.Fatalf("disable changed other settings: %+v %v %v", current, changed, err)
				}
			} else if !errors.Is(err, failure) || changed || current != previous {
				t.Fatalf("disable failed rollback: %+v %v %v", current, changed, err)
			}
		})
	}
}

func TestGuardianRechecksForeignProxyAtWriteBoundary(t *testing.T) {
	expected := ProxySettings{Hostname: "127.0.0.1", Port: "2023"}
	path := filepath.Join(t.TempDir(), "owner")
	if err := writeProxyOwner(path, "token"); err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []windowsProxyConnection{
		{Flags: 3, Server: "127.0.0.1:7897"},
		{Flags: 3, Server: "http=127.0.0.1:2023;https=127.0.0.1:7897"},
		{Flags: 1, Server: "127.0.0.1:2023"},
	} {
		connection := windowsProxyConnectionAPI{
			read:   func() (windowsProxyConnection, error) { return foreign, nil },
			write:  func(windowsProxyConnection) error { t.Fatal("must not disable changed proxy"); return nil },
			notify: func() error { t.Fatal("must not notify"); return nil },
		}
		changed, err := cleanupProxyOwner(path, "token", expected,
			func() (bool, string, error) { return true, "127.0.0.1:2023", nil },
			func() (bool, error) { return disableWindowsProxy(connection, &expected) },
		)
		if changed || err != nil {
			t.Fatalf("foreign cleanup: %v %v", changed, err)
		}
	}
}

func TestWindowsProxyConnectionNativeLayout(t *testing.T) {
	option := internetPerConnOption{}
	list := internetPerConnOptionList{}
	wantOptionSize, wantValueOffset, wantListSize := uintptr(16), uintptr(8), uintptr(32)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		wantOptionSize, wantValueOffset, wantListSize = 12, 4, 20
	}
	if unsafe.Sizeof(option) != wantOptionSize || unsafe.Offsetof(option.Value) != wantValueOffset || unsafe.Sizeof(list) != wantListSize {
		t.Fatalf("incorrect WinINET ABI: option=%d value=%d list=%d", unsafe.Sizeof(option), unsafe.Offsetof(option.Value), unsafe.Sizeof(list))
	}
}
