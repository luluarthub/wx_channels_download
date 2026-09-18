package api

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/viper"
	"wx_channel/internal/config"
)

type testSystemProxyController struct {
	enabled bool
	err     error
	calls   []bool
}

func (p *testSystemProxyController) ProxySetSystem() bool { return p.enabled }
func (p *testSystemProxyController) SetSystemProxy(value bool) error {
	p.calls = append(p.calls, value)
	if p.err != nil {
		return p.err
	}
	p.enabled = value
	return nil
}

func TestSystemProxyAPIUpdatesRuntimeAndConfiguration(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("proxy.system", false)
	path := filepath.Join(t.TempDir(), "config.yaml")
	controller := &testSystemProxyController{}
	c := &APIClient{cfg: &APIConfig{Original: &config.Config{FullPath: path}}, system_proxy_controller: controller}
	if err := c.changeSystemProxy(true); err != nil {
		t.Fatal(err)
	}
	if !controller.enabled || !viper.GetBool("proxy.system") {
		t.Fatal("runtime/config did not enable together")
	}
	if err := c.changeSystemProxy(false); err != nil {
		t.Fatal(err)
	}
	if controller.enabled || viper.GetBool("proxy.system") {
		t.Fatal("runtime/config did not disable together")
	}
}

func TestSystemProxyAPISaveFailureRollsBackRuntime(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("proxy.system", false)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	controller := &testSystemProxyController{}
	c := &APIClient{cfg: &APIConfig{Original: &config.Config{FullPath: filepath.Join(blocked, "config.yaml")}}, system_proxy_controller: controller}
	if err := c.changeSystemProxy(true); err == nil {
		t.Fatal("expected save failure")
	}
	if controller.enabled || viper.GetBool("proxy.system") || !reflect.DeepEqual(controller.calls, []bool{true, false}) {
		t.Fatalf("failed rollback: runtime=%v config=%v calls=%v", controller.enabled, viper.GetBool("proxy.system"), controller.calls)
	}
}

func TestSystemProxyAPIDoesNotSaveWhenListenerUnavailable(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("proxy.system", false)
	path := filepath.Join(t.TempDir(), "config.yaml")
	failure := errors.New("proxy service is not running")
	controller := &testSystemProxyController{err: failure}
	c := &APIClient{cfg: &APIConfig{Original: &config.Config{FullPath: path}}, system_proxy_controller: controller}
	if err := c.changeSystemProxy(true); !errors.Is(err, failure) {
		t.Fatalf("got %v", err)
	}
	if controller.enabled || viper.GetBool("proxy.system") {
		t.Fatal("failure changed proxy state")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("configuration unexpectedly saved: %v", err)
	}
}
