//go:build windows

package system

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Some Windows installations update Connections but leave these older fields
// unchanged after InternetSetOption(75). WinHTTP's current-user IE query uses
// them, so retain a separate snapshot and narrowly synchronize both views.
type windowsProxyLegacy struct {
	Enabled       uint32
	Server        string
	EnablePresent bool
	ServerPresent bool
}

type windowsProxyStateStore struct {
	readConnection  func() (windowsProxyConnection, error)
	writeConnection func(windowsProxyConnection) error
	readLegacy      func() (windowsProxyLegacy, error)
	writeLegacy     func(windowsProxyLegacy) error
	readEffective   func() (windowsProxyConnection, error)
	readIEProxy     func() (string, error)
}

func (s windowsProxyStateStore) read() (windowsProxyConnection, error) {
	state, err := s.readConnection()
	if err != nil {
		return state, err
	}
	state.Legacy, err = s.readLegacy()
	return state, err
}

func (s windowsProxyStateStore) write(state windowsProxyConnection) error {
	if err := s.writeConnection(state); err != nil {
		return err
	}
	return s.writeLegacy(state.Legacy)
}

func (s windowsProxyStateStore) restore(state windowsProxyConnection) error {
	// Attempt both independent restores even if one of them fails.
	return errors.Join(s.writeConnection(state), s.writeLegacy(state.Legacy))
}

func (s windowsProxyStateStore) verify(expected windowsProxyConnection) error {
	actual, err := s.read()
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("Windows proxy connection and legacy settings did not retain the requested values")
	}
	effective, err := s.readEffective()
	if err != nil {
		return err
	}
	enabled := expected.Flags&proxyTypeProxy != 0
	if (effective.Flags&proxyTypeProxy != 0) != enabled || (enabled && effective.Server != expected.Server) {
		return errors.New("Windows effective proxy connection disagrees with the requested settings")
	}
	ieProxy, err := s.readIEProxy()
	if err != nil {
		return err
	}
	wantIEProxy := ""
	if enabled {
		wantIEProxy = expected.Server
	}
	if ieProxy != wantIEProxy {
		return errors.New("WinHTTP current-user proxy disagrees with the requested settings")
	}
	return nil
}

const windowsInternetSettingsPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func readWindowsProxyLegacy() (windowsProxyLegacy, error) {
	var state windowsProxyLegacy
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsInternetSettingsPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer key.Close()
	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return state, err
	}
	state.Enabled, state.EnablePresent = uint32(enabled), err == nil
	state.Server, _, err = key.GetStringValue("ProxyServer")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return state, err
	}
	state.ServerPresent = err == nil
	return state, nil
}

func writeWindowsProxyLegacy(state windowsProxyLegacy) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsInternetSettingsPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	remove := func(name string) error {
		err := key.DeleteValue(name)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	if state.ServerPresent {
		err = key.SetStringValue("ProxyServer", state.Server)
	} else {
		err = remove("ProxyServer")
	}
	if err != nil {
		return err
	}
	if state.EnablePresent {
		return key.SetDWordValue("ProxyEnable", state.Enabled)
	}
	return remove("ProxyEnable")
}

// Every enabled view must still be owned. A foreign proxy in either view must
// be preserved, including while another application is updating only one view.
func windowsProxyStateAddresses(state windowsProxyConnection) (bool, string) {
	var servers []string
	if state.Flags&proxyTypeProxy != 0 {
		servers = append(servers, state.Server)
	}
	if state.Legacy.EnablePresent && state.Legacy.Enabled != 0 {
		servers = append(servers, state.Legacy.Server)
	}
	for _, server := range servers {
		if strings.TrimSpace(server) == "" {
			return true, ""
		}
	}
	return len(servers) != 0, strings.Join(servers, ";")
}

func windowsProxyStateMatches(state windowsProxyConnection, expected ProxySettings) bool {
	enabled, servers := windowsProxyStateAddresses(state)
	return enabled && allProxyAddressesMatch(servers, expected)
}

type windowsIEProxyConfig struct {
	AutoDetect    uint32
	AutoConfigURL *uint16
	Proxy         *uint16
	Bypass        *uint16
}

var winHTTPGetIEProxyConfig = windows.NewLazySystemDLL("winhttp.dll").NewProc("WinHttpGetIEProxyConfigForCurrentUser")

func readWindowsIEProxy() (string, error) {
	var config windowsIEProxyConfig
	result, _, callErr := winHTTPGetIEProxyConfig.Call(uintptr(unsafe.Pointer(&config)))
	for _, pointer := range []*uint16{config.AutoConfigURL, config.Proxy, config.Bypass} {
		if pointer != nil {
			defer proxyGlobalFree.Call(uintptr(unsafe.Pointer(pointer)))
		}
	}
	if result == 0 {
		return "", fmt.Errorf("query WinHTTP current-user proxy: %w", callErr)
	}
	return windows.UTF16PtrToString(config.Proxy), nil
}
