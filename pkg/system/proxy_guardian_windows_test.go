//go:build windows

package system

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAllProxyAddressesMatch(t *testing.T) {
	expected := ProxySettings{Hostname: "127.0.0.1", Port: "2023"}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"127.0.0.1:2023", true},
		{"http=127.0.0.1:2023; https=127.0.0.1:2023;", true},
		{"http=127.0.0.1:2023;https=127.0.0.1:7897", false},
		{"http=127.0.0.1:2023;socks=other:1080", false},
		{"", false}, {"http=", false}, {"127.0.0.1:20230", false},
	} {
		if got := allProxyAddressesMatch(tc.value, expected); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.value, got, tc.want)
		}
	}
	if !allProxyAddressesMatch("[::1]:2023", ProxySettings{Hostname: "::1", Port: "2023"}) {
		t.Fatal("IPv6 match failed")
	}
}

func TestGuardianOwnershipProtectsNewInstanceAndOtherProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-owner")
	expected := ProxySettings{Hostname: "127.0.0.1", Port: "2023"}
	for _, tc := range []struct {
		name, owner, token, server string
		enabled, want              bool
	}{
		{"crashed current instance", "old", "old", "127.0.0.1:2023", true, true},
		{"new instance same address", "new", "old", "127.0.0.1:2023", true, false},
		{"user changed proxy", "old", "old", "127.0.0.1:7897", true, false},
		{"mixed protocols", "old", "old", "http=127.0.0.1:2023;https=127.0.0.1:7897", true, false},
		{"already disabled", "old", "old", "127.0.0.1:2023", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := writeProxyOwner(path, tc.owner); err != nil {
				t.Fatal(err)
			}
			calls := 0
			changed, err := cleanupProxyOwner(path, tc.token, expected,
				func() (bool, string, error) { return tc.enabled, tc.server, nil },
				func() (bool, error) { calls++; return true, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tc.want || (calls == 1) != tc.want {
				t.Fatalf("changed=%v calls=%d, want %v", changed, calls, tc.want)
			}
		})
	}
}

func TestGuardianReadFailuresNeverDisableProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-owner")
	failure := errors.New("registry unavailable")
	disable := func() (bool, error) { t.Fatal("must not disable"); return false, nil }
	read := func() (bool, string, error) { return false, "", failure }
	if changed, err := cleanupProxyOwner(path, "token", ProxySettings{}, read, disable); changed || err != nil {
		t.Fatalf("missing owner: %v %v", changed, err)
	}
	if err := os.WriteFile(path, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := cleanupProxyOwner(path, "token", ProxySettings{}, read, disable); changed || !errors.Is(err, failure) {
		t.Fatalf("read failure: %v %v", changed, err)
	}
}

func TestGuardianLockSerializesClaimAndCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-owner")
	unlock, err := lockProxyOwnership(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeProxyOwner(path, "old"); err != nil {
		unlock()
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		release, err := lockProxyOwnership(path)
		if err != nil {
			done <- err
			return
		}
		defer release()
		changed, err := cleanupProxyOwner(path, "old", ProxySettings{Hostname: "127.0.0.1", Port: "2023"},
			func() (bool, string, error) { return true, "127.0.0.1:2023", nil },
			func() (bool, error) { return false, errors.New("old guardian disabled new instance") },
		)
		if changed {
			err = errors.New("old guardian changed proxy")
		}
		done <- err
	}()
	<-started
	if err := writeProxyOwner(path, "new"); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
