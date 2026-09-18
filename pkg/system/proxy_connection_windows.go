//go:build windows

package system

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	internetOptionPerConnection = 75
	internetPerConnFlags        = 1
	internetPerConnProxyServer  = 2
	internetPerConnFlagsUI      = 10
	proxyTypeDirect             = 1
	proxyTypeProxy              = 2
)

var internetQueryOption = windows.NewLazySystemDLL("wininet.dll").NewProc("InternetQueryOptionW")
var proxyGlobalFree = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFree")

// Value is the native DWORD/pointer/FILETIME union (8 bytes). Go's uint64
// alignment gives the required 12-byte x86 / 16-byte x64 option structure.
type internetPerConnOption struct {
	Option uint32
	Value  uint64
}

type internetPerConnOptionList struct {
	Size       uint32
	Connection *uint16 // nil selects the default/LAN connection
	Count      uint32
	Error      uint32
	Options    *internetPerConnOption
}

type windowsProxyConnection struct {
	Flags  uint32
	Server string
}

type windowsProxyConnectionAPI struct {
	read   func() (windowsProxyConnection, error)
	write  func(windowsProxyConnection) error
	notify func() error
}

func nativeWindowsProxyConnectionAPI() windowsProxyConnectionAPI {
	return windowsProxyConnectionAPI{
		read: readWindowsProxyConnection, write: writeWindowsProxyConnection,
		notify: notify_proxy_settings_changed,
	}
}

// Query the user-selected flags, falling back for older WinINET versions.
// Bypass/PAC URLs are never written, so the user's settings are preserved.
func readWindowsProxyConnection() (windowsProxyConnection, error) {
	state, err := queryWindowsProxyConnection(internetPerConnFlagsUI)
	if err != nil {
		return queryWindowsProxyConnection(internetPerConnFlags)
	}
	return state, nil
}

func queryWindowsProxyConnection(flagsOption uint32) (windowsProxyConnection, error) {
	options := [2]internetPerConnOption{{Option: flagsOption}, {Option: internetPerConnProxyServer}}
	list := internetPerConnOptionList{Count: uint32(len(options)), Options: &options[0]}
	list.Size = uint32(unsafe.Sizeof(list))
	size := list.Size
	result, _, callErr := internetQueryOption.Call(0, internetOptionPerConnection,
		uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&size)))
	runtime.KeepAlive(options)
	// Returned strings, including partial query results, belong to GlobalAlloc.
	if options[1].Value != 0 {
		defer proxyGlobalFree.Call(uintptr(options[1].Value))
	}
	if result == 0 {
		return windowsProxyConnection{}, fmt.Errorf("query Windows proxy connection (option %d): %w", list.Error, callErr)
	}
	server := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(uintptr(options[1].Value))))
	return windowsProxyConnection{Flags: uint32(options[0].Value), Server: server}, nil
}

func writeWindowsProxyConnection(state windowsProxyConnection) error {
	server, err := windows.UTF16PtrFromString(state.Server)
	if err != nil {
		return err
	}
	options := [2]internetPerConnOption{
		{Option: internetPerConnFlags, Value: uint64(state.Flags)},
		{Option: internetPerConnProxyServer, Value: uint64(uintptr(unsafe.Pointer(server)))},
	}
	list := internetPerConnOptionList{Count: uint32(len(options)), Options: &options[0]}
	list.Size = uint32(unsafe.Sizeof(list))
	result, _, callErr := internet_set_option.Call(0, internetOptionPerConnection,
		uintptr(unsafe.Pointer(&list)), uintptr(list.Size))
	runtime.KeepAlive(server)
	runtime.KeepAlive(options)
	if result == 0 {
		return fmt.Errorf("set Windows proxy connection (option %d): %w", list.Error, callErr)
	}
	return nil
}
