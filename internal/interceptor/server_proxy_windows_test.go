package interceptor

import "testing"

func TestStoppedServerCanDisableSystemProxy(t *testing.T) {
	// This empty ownership directory prevents access to the user's registry.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	s := &InterceptorServer{Interceptor: &Interceptor{Settings: &InterceptorConfig{
		ProxySetSystem: true, ProxyServerHostname: "127.0.0.1", ProxyServerPort: 52023,
	}}}
	if err := s.SetSystemProxy(false); err != nil {
		t.Fatalf("disable stopped server: %v", err)
	}
	if s.ProxySetSystem() {
		t.Fatal("system proxy preference was not disabled")
	}
	if err := s.SetSystemProxy(true); err == nil {
		t.Fatal("enabling a stopped listener must be rejected")
	}
	if s.ProxySetSystem() {
		t.Fatal("rejected enable changed the preference")
	}
}
