package interceptor

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestProxyLifecycleFailureRollback(t *testing.T) {
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name                   string
		startErr, configureErr error
		want                   []string
	}{
		{"listener failed", failure, nil, []string{"start", "cleanup"}},
		{"system proxy failed", nil, failure, []string{"start", "configure", "cleanup"}},
		{"success", nil, nil, []string{"start", "configure"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			err := startProxyLifecycle(
				func() error { calls = append(calls, "start"); return tc.startErr },
				func() error { calls = append(calls, "configure"); return tc.configureErr },
				func() error { calls = append(calls, "cleanup"); return nil },
			)
			if !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("calls %v, want %v", calls, tc.want)
			}
			if (tc.startErr != nil || tc.configureErr != nil) && !errors.Is(err, failure) {
				t.Fatalf("failure lost: %v", err)
			}
			if tc.startErr == nil && tc.configureErr == nil && err != nil {
				t.Fatal(err)
			}
		})
	}
}

type closeTrackingProxy struct{ closed bool }

func (p *closeTrackingProxy) Start(int) error                              { return nil }
func (p *closeTrackingProxy) Close() error                                 { p.closed = true; return nil }
func (p *closeTrackingProxy) AddPlugin(interface{})                        {}
func (p *closeTrackingProxy) ServeHTTP(http.ResponseWriter, *http.Request) {}

func TestProxyStopClosesWithoutSystemProxy(t *testing.T) {
	client := &closeTrackingProxy{}
	c := &Interceptor{Settings: &InterceptorConfig{ProxyTun: true}, proxy: client}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if !client.closed {
		t.Fatal("proxy was not closed")
	}
}

func TestProxyStopClosesEvenIfProxyCleanupFails(t *testing.T) {
	registryErr := errors.New("registry unavailable")
	closeErr := errors.New("listener close error")
	closed := false
	err := stopProxyLifecycle(func() error { return registryErr }, func() error { closed = true; return closeErr })
	if !closed || !errors.Is(err, registryErr) || !errors.Is(err, closeErr) {
		t.Fatalf("closed=%v, error=%v", closed, err)
	}
}
