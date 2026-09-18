//go:build windows

package system

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

const proxyGuardianEnv = "WX_CHANNELS_PROXY_GUARDIAN"

var proxyGuardianState struct {
	sync.Mutex
	token   string
	started map[string]bool
}

type proxyGuardianRequest struct {
	ParentHandle uintptr
	Token        string
	Expected     ProxySettings
}

func proxyProcessToken() (string, error) {
	proxyGuardianState.Lock()
	defer proxyGuardianState.Unlock()
	if proxyGuardianState.token == "" {
		var value [32]byte
		if _, err := rand.Read(value[:]); err != nil {
			return "", err
		}
		proxyGuardianState.token = hex.EncodeToString(value[:])
	}
	return proxyGuardianState.token, nil
}

// StartProxyGuardian inherits a real process handle, avoiding both a startup
// race when the parent dies immediately and waiting on a reused process ID.
func StartProxyGuardian(parentPID int, expected ProxySettings) error {
	token, err := proxyProcessToken()
	if err != nil {
		return err
	}
	expected = merge_default_settings(expected)
	key := fmt.Sprintf("%d|%s|%s", parentPID, expected.Hostname, expected.Port)
	proxyGuardianState.Lock()
	defer proxyGuardianState.Unlock()
	if proxyGuardianState.started[key] {
		return nil
	}
	parent, err := windows.OpenProcess(windows.SYNCHRONIZE, true, uint32(parentPID))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	request, err := json.Marshal(proxyGuardianRequest{uintptr(parent), token, expected})
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable)
	cmd.Env = append(os.Environ(), proxyGuardianEnv+"="+string(request))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              windows.CREATE_NO_WINDOW,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(parent)},
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if proxyGuardianState.started == nil {
		proxyGuardianState.started = make(map[string]bool)
	}
	proxyGuardianState.started[key] = true
	return cmd.Process.Release()
}

// RunProxyGuardianFromEnv must run before normal application initialization.
func RunProxyGuardianFromEnv() bool {
	raw := os.Getenv(proxyGuardianEnv)
	if raw == "" {
		return false
	}
	var request proxyGuardianRequest
	if json.Unmarshal([]byte(raw), &request) != nil || request.ParentHandle == 0 || len(request.Token) != 64 {
		return true
	}
	parent := windows.Handle(request.ParentHandle)
	defer windows.CloseHandle(parent)
	result, err := windows.WaitForSingleObject(parent, windows.INFINITE)
	if err == nil && result == windows.WAIT_OBJECT_0 {
		_, _ = disableOwnedWindowsProxy(request.Expected, request.Token)
	}
	return true
}

func proxyOwnershipPath() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "wx_channels_download", "proxy-owner"), nil
}

// All cooperating instances hold this user-scoped lock while claiming or
// clearing the global proxy. A Windows mutex belongs to an OS thread.
func lockProxyOwnership(path string) (func(), error) {
	hash := sha256.Sum256([]byte(strings.ToLower(path)))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`Local\wx_channels_download_proxy_%x`, hash[:16]))
	if err != nil {
		return nil, err
	}
	mutex, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, err
	}
	runtime.LockOSThread()
	result, err := windows.WaitForSingleObject(mutex, 30000)
	if err != nil || (result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED) {
		runtime.UnlockOSThread()
		windows.CloseHandle(mutex)
		return nil, fmt.Errorf("waiting for proxy ownership lock: result=%d: %v", result, err)
	}
	return func() {
		windows.ReleaseMutex(mutex)
		windows.CloseHandle(mutex)
		runtime.UnlockOSThread()
	}, nil
}

func writeProxyOwner(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".proxy-owner-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(token)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func disable_proxy_if_matches(expected ProxySettings) (bool, error) {
	token, err := proxyProcessToken()
	if err != nil {
		return false, err
	}
	return disableOwnedWindowsProxy(expected, token)
}

func disableOwnedWindowsProxy(expected ProxySettings, token string) (bool, error) {
	path, err := proxyOwnershipPath()
	if err != nil {
		return false, err
	}
	unlock, err := lockProxyOwnership(path)
	if err != nil {
		return false, err
	}
	defer unlock()
	return cleanupProxyOwner(path, token, expected, readWindowsProxySnapshot, func() (bool, error) {
		return disableWindowsProxy(nativeWindowsProxyConnectionAPI(), &expected)
	})
}

// The injectable connection boundary keeps ownership tests off the user's proxy.
func cleanupProxyOwner(path, token string, expected ProxySettings, read func() (bool, string, error), disable func() (bool, error)) (bool, error) {
	owner, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if token == "" || string(owner) != token {
		return false, nil
	}
	enabled, server, err := read()
	if err != nil {
		return false, err
	}
	if !enabled || !allProxyAddressesMatch(server, expected) {
		return false, nil
	}
	return disable()
}

func readWindowsProxySnapshot() (bool, string, error) {
	state, err := readWindowsProxyConnection()
	return state.Flags&proxyTypeProxy != 0, state.Server, err
}

// Reading only the HTTP entry would also disable unrelated HTTPS/SOCKS proxies.
func allProxyAddressesMatch(server string, expected ProxySettings) bool {
	matched := false
	for _, entry := range strings.Split(server, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if at := strings.IndexByte(entry, '='); at >= 0 {
			entry = strings.TrimSpace(entry[at+1:])
		}
		host, port, err := split_host_port(entry)
		if err != nil || !same_proxy_address(ProxySettings{Hostname: host, Port: port}, expected) {
			return false
		}
		matched = true
	}
	return matched
}
