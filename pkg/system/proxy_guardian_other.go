//go:build !windows

package system

func StartProxyGuardian(parentPID int, expected ProxySettings) error { return nil }

func RunProxyGuardianFromEnv() bool { return false }

func disable_proxy_if_matches(expected ProxySettings) (bool, error) {
	return disable_matching_proxy_address(expected)
}
