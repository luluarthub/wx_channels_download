package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"wx_channel/pkg/scraper/zhihu"
)

type httpTestSphBackend struct{ calls int }

func (b *httpTestSphBackend) DeploySphWorker(context.Context) (*SphDeployResult, error) {
	b.calls++
	return &SphDeployResult{WorkerName: "in-memory-test", WorkerURL: "https://example.invalid"}, nil
}

type httpTestZhihuBackend struct {
	ZhihuCollectionReader
	calls int
}

func (b *httpTestZhihuBackend) FetchCurrentUser() (*zhihu.User, error) {
	b.calls++
	return &zhihu.User{ID: "test-id", Name: "test-user", URLToken: "test-user"}, nil
}

type httpTestCredentialBackend struct{ calls int }

func (b *httpTestCredentialBackend) HeaderForURL(string) (string, error) {
	b.calls++
	return "z_c0=in-memory-test", nil
}

func TestHTTPTransportRetainsConfiguredBackends(t *testing.T) {
	sph := &httpTestSphBackend{}
	zhihuBackend := &httpTestZhihuBackend{}
	credentials := &httpTestCredentialBackend{}
	server, err := NewServer(Config{SphDeployer: sph, ZhihuCollections: zhihuBackend, ZhihuCredentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(server)
	rpc := func(method string, params any) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("MCP-Protocol-Version", "2025-06-18")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var reply struct {
			Result map[string]any  `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || len(reply.Error) != 0 || reply.Result["isError"] == true {
			t.Fatalf("RPC %s failed: status=%d body=%s", method, recorder.Code, recorder.Body.String())
		}
		return reply.Result
	}

	listed := rpc("tools/list", map[string]any{})
	tools, ok := listed["tools"].([]any)
	if !ok {
		t.Fatalf("missing tools: %v", listed)
	}
	names := make(map[string]bool)
	for _, raw := range tools {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	if len(tools) != len(server.tool_definitions()) {
		t.Fatalf("HTTP exposed %d tools, configured server has %d", len(tools), len(server.tool_definitions()))
	}
	for _, name := range []string{"deploy_sph_worker", "get_zhihu_credential_status", "get_my_zhihu_collections", "get_zhihu_collection_contents", "get_my_zhihu_answers", "get_my_zhihu_posts", "get_my_zhihu_zvideos", "get_my_zhihu_columns"} {
		if !names[name] {
			t.Errorf("HTTP transport dropped configured tool %s", name)
		}
	}

	// These calls use only in-memory stubs: no real cookies, network requests or deployments.
	status := rpc("tools/call", map[string]any{"name": "get_zhihu_credential_status", "arguments": map[string]any{}})
	if status["structuredContent"].(map[string]any)["authenticated"] != true || credentials.calls != 1 || zhihuBackend.calls != 1 {
		t.Fatalf("HTTP credential/user backends not called: status=%v credentials=%d user=%d", status, credentials.calls, zhihuBackend.calls)
	}
	deployed := rpc("tools/call", map[string]any{"name": "deploy_sph_worker", "arguments": map[string]any{}})
	if deployed["structuredContent"].(map[string]any)["worker_name"] != "in-memory-test" || sph.calls != 1 {
		t.Fatalf("HTTP SPH backend not called: response=%v calls=%d", deployed, sph.calls)
	}
}
