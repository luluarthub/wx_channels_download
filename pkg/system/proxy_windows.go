//go:build windows

package system

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var internet_set_option = windows.NewLazySystemDLL("wininet.dll").NewProc("InternetSetOptionW")

const (
	internet_option_refresh          = 37
	internet_option_settings_changed = 39
)

func enable_proxy(args ProxySettings) error {
	args = merge_default_settings(args)
	owner_path, err := proxyOwnershipPath()
	if err != nil {
		return err
	}
	unlock, err := lockProxyOwnership(owner_path)
	if err != nil {
		return err
	}
	defer unlock()
	token, err := proxyProcessToken()
	if err != nil {
		return err
	}
	if err := StartProxyGuardian(os.Getpid(), args); err != nil {
		return fmt.Errorf("failed to start proxy guardian: %w", err)
	}
	return configureWindowsProxy(args, owner_path, token, nativeWindowsProxyConnectionAPI())
}

// Called under the ownership mutex. Snapshot before claiming, and restore both
// connection settings and the previous owner if any mutation/notification fails.
func configureWindowsProxy(args ProxySettings, ownerPath, token string, connection windowsProxyConnectionAPI) (resultErr error) {
	previous, err := connection.read()
	if err != nil {
		return err
	}
	oldOwner, ownerErr := os.ReadFile(ownerPath)
	if ownerErr != nil && !errors.Is(ownerErr, os.ErrNotExist) {
		return ownerErr
	}
	if err := writeProxyOwner(ownerPath, token); err != nil {
		return err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		restoreErr := connection.write(previous)
		var restoreOwnerErr error
		if restoreErr == nil {
			if errors.Is(ownerErr, os.ErrNotExist) {
				restoreOwnerErr = os.Remove(ownerPath)
			} else {
				restoreOwnerErr = writeProxyOwner(ownerPath, string(oldOwner))
			}
		}
		resultErr = errors.Join(resultErr, restoreErr, restoreOwnerErr, connection.notify())
	}()
	next := previous
	next.Flags |= proxyTypeProxy
	next.Server = net.JoinHostPort(args.Hostname, args.Port)
	if err := connection.write(next); err != nil {
		return err
	}
	return connection.notify()
}

func disable_proxy(args ProxySettings) error {
	path, err := proxyOwnershipPath()
	if err != nil {
		return err
	}
	unlock, err := lockProxyOwnership(path)
	if err != nil {
		return err
	}
	defer unlock()
	return disable_proxy_unlocked(args)
}

func disable_proxy_unlocked(args ProxySettings) error {
	_, err := disableWindowsProxy(nativeWindowsProxyConnectionAPI(), nil)
	return err
}

func disableWindowsProxy(connection windowsProxyConnectionAPI, expected *ProxySettings) (changed bool, resultErr error) {
	previous, err := connection.read()
	if err != nil {
		return false, err
	}
	// Recheck the exact snapshot used for the write: external proxy managers
	// do not participate in our ownership mutex and may change the address.
	if expected != nil && (previous.Flags&proxyTypeProxy == 0 || !allProxyAddressesMatch(previous.Server, *expected)) {
		return false, nil
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, connection.write(previous), connection.notify())
		}
	}()
	next := previous
	next.Flags &^= proxyTypeProxy
	if next.Flags == 0 {
		next.Flags = proxyTypeDirect
	}
	if err := connection.write(next); err != nil {
		return false, err
	}
	if err := connection.notify(); err != nil {
		return false, err
	}
	return true, nil
}

func notify_proxy_settings_changed() error {
	if err := internet_set_option.Find(); err != nil {
		return fmt.Errorf("failed to load InternetSetOptionW: %w", err)
	}
	for _, option := range []uintptr{internet_option_settings_changed, internet_option_refresh} {
		result, _, call_err := internet_set_option.Call(0, option, 0, 0)
		if result == 0 {
			return fmt.Errorf("failed to refresh Windows proxy settings: %w", call_err)
		}
	}
	return nil
}

// run_reg_command executes a "reg" command (e.g. "reg add ...").
// If the direct call fails (e.g. due to group policy lock), it retries with
// elevated privileges via PowerShell Start-Process -Verb RunAs -Wait.
// This keeps the calling process (HTTP server) alive while only elevating
// the individual registry operation through a UAC dialog.
func run_reg_command(args ...string) error {
	// Attempt 1: direct reg call (no elevation, no UAC prompt)
	cmd := exec.Command("reg", args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	// Attempt 2: elevate just this reg command via PowerShell.
	// Start-Process accepts -ArgumentList only once, so join the arguments into
	// a single string, double-quoting each so reg.exe parses them correctly.
	arg_list := strings.Join(quote_args(args), " ")
	psCmd := "$process = Start-Process -Verb RunAs -Wait -PassThru -WindowStyle Hidden -FilePath 'reg' -ArgumentList " + powershell_escape(arg_list) + "; exit $process.ExitCode"

	psExec := exec.Command("powershell", "-NoProfile", "-Command", psCmd)
	output2, err2 := psExec.CombinedOutput()
	if err2 != nil {
		return fmt.Errorf(
			"普通执行失败: %s\n提权执行失败: %s",
			strings.TrimSpace(string(output)),
			strings.TrimSpace(string(output2)),
		)
	}
	return nil
}

// powershell_escape wraps a string in single quotes for use in PowerShell
// -ArgumentList, doubling any embedded single quotes per PS escaping rules.
func powershell_escape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// quote_args double-quotes each argument for Windows command-line parsing,
// doubling any embedded double quotes.
func quote_args(args []string) []string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = `"` + strings.ReplaceAll(arg, `"`, `""`) + `"`
	}
	return quoted
}

func fetch_cur_proxy(args ProxySettings) (*ProxySettings, error) {
	state, err := readWindowsProxyConnection()
	if err != nil {
		return nil, err
	}
	if state.Flags&proxyTypeProxy == 0 || state.Server == "" {
		return nil, nil
	}
	host, port, err := parse_proxy_server_value(state.Server)
	if err != nil {
		return nil, err
	}
	if host == "" || port == "" {
		return nil, nil
	}
	return &ProxySettings{
		Hostname: host,
		Port:     port,
	}, nil
}

func get_network_interfaces() (*HardwarePort, error) {
	return nil, errors.New("not support")
}

// ProxyTargetDescription has nothing to report on Windows: the proxy lives in the registry and
// applies globally, so there is no network service to pick and no way to pick the wrong one.
func ProxyTargetDescription(configured string) (service string, warning string) {
	return "", ""
}

func read_reg_value(path string, name string) (string, error) {
	// Native reads preserve whitespace and distinguish a missing value without
	// depending on the language/code page of reg.exe's error messages.
	if !strings.HasPrefix(path, `HKCU\`) {
		return "", fmt.Errorf("unsupported registry path: %s", path)
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, strings.TrimPrefix(path, `HKCU\`), registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer key.Close()
	var value string
	if name == "ProxyEnable" {
		var number uint64
		number, _, err = key.GetIntegerValue(name)
		value = strconv.FormatUint(number, 10)
	} else {
		value, _, err = key.GetStringValue(name)
	}
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return value, err
}

func parse_reg_dword(value string) (int64, error) {
	num, err := strconv.ParseInt(strings.TrimSpace(value), 0, 64)
	if err != nil {
		return 0, fmt.Errorf("解析系统代理开关失败: %v", err)
	}
	return num, nil
}

func parse_proxy_server_value(value string) (string, string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", "", nil
	}
	parts := strings.Split(raw, ";")
	candidate := pick_proxy_candidate(parts, "http=")
	if candidate == "" {
		candidate = pick_proxy_candidate(parts, "https=")
	}
	if candidate == "" {
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if idx := strings.Index(part, "="); idx >= 0 {
				candidate = strings.TrimSpace(part[idx+1:])
				break
			}
		}
	}
	if candidate == "" {
		candidate = raw
	}
	return split_host_port(candidate)
}

func pick_proxy_candidate(parts []string, prefix string) string {
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), prefix) {
			return strings.TrimSpace(part[len(prefix):])
		}
	}
	return ""
}

func split_host_port(value string) (string, string, error) {
	candidate := strings.TrimSpace(value)
	if candidate == "" {
		return "", "", nil
	}
	if strings.HasPrefix(candidate, "[") {
		host, port, err := net.SplitHostPort(candidate)
		if err != nil {
			return "", "", fmt.Errorf("解析系统代理地址失败: %v", err)
		}
		return host, port, nil
	}
	idx := strings.LastIndex(candidate, ":")
	if idx <= 0 || idx == len(candidate)-1 {
		return "", "", fmt.Errorf("解析系统代理地址失败: %s", candidate)
	}
	return candidate[:idx], candidate[idx+1:], nil
}
