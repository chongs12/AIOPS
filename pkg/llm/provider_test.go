package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// fakeOpenAI 起一个 OpenAI 兼容的假后端，记录请求并按配置返回。
type fakeOpenAI struct {
	srv     *httptest.Server
	calls   atomic.Int32
	handler http.HandlerFunc
}

func newFakeOpenAI(t *testing.T, handler http.HandlerFunc) *fakeOpenAI {
	f := &fakeOpenAI{handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// okHandler 返回固定 content 的正常响应。
func okHandler(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": content},
			}},
			"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 7},
		})
	}
}

func errHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"backend boom"}}`))
	}
}

// readRequest 解析收到的请求体。
func readRequest(t *testing.T, r *http.Request) (payload chatPayload, auth string) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	return payload, r.Header.Get("Authorization")
}

func TestProviderRequestShape(t *testing.T) {
	f := newFakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		payload, auth := readRequest(t, r)
		if r.URL.Path != "/chat/completions" {
			t.Errorf("路径 = %s", r.URL.Path)
		}
		if auth != "Bearer test-key" {
			t.Errorf("Authorization = %q", auth)
		}
		if payload.Model != "glm-4.5" || payload.Temperature != 0.2 || payload.MaxTokens != 4096 {
			t.Errorf("payload 异常: %+v", payload)
		}
		if len(payload.Messages) != 1 || payload.Messages[0].Content != "你好" {
			t.Errorf("messages 异常: %+v", payload.Messages)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"好"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	p := NewOpenAIProvider(OpenAIOptions{
		Name: "test", BaseURL: f.srv.URL, APIKey: "test-key",
		Model: "glm-4.5", MaxTokens: 4096, Temperature: 0.2,
	})
	resp, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "你好"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "好" || resp.PromptTokens != 1 || resp.Provider != "test" {
		t.Errorf("resp 异常: %+v", resp)
	}
}

func TestProviderHTTPError(t *testing.T) {
	f := newFakeOpenAI(t, errHandler(http.StatusBadGateway))
	p := NewOpenAIProvider(OpenAIOptions{Name: "bad", BaseURL: f.srv.URL, APIKey: "k", Model: "m"})
	if _, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}}); err == nil {
		t.Fatal("HTTP 502 应报错")
	}
}

func TestClientFallbackChain(t *testing.T) {
	primary := newFakeOpenAI(t, errHandler(http.StatusInternalServerError))
	backup := newFakeOpenAI(t, okHandler("from-backup"))

	c, err := NewClient([]Provider{
		NewOpenAIProvider(OpenAIOptions{Name: "primary", BaseURL: primary.srv.URL, APIKey: "k", Model: "m"}),
		NewOpenAIProvider(OpenAIOptions{Name: "backup", BaseURL: backup.srv.URL, APIKey: "k", Model: "m"}),
	}, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("fallback 应成功: %v", err)
	}
	if resp.Content != "from-backup" {
		t.Errorf("应来自 backup: %+v", resp)
	}
	if primary.calls.Load() != 1 || backup.calls.Load() != 1 {
		t.Errorf("调用次数异常 primary=%d backup=%d", primary.calls.Load(), backup.calls.Load())
	}
}

func TestClientAllFail(t *testing.T) {
	p1 := newFakeOpenAI(t, errHandler(http.StatusBadGateway))
	p2 := newFakeOpenAI(t, errHandler(http.StatusServiceUnavailable))
	c, _ := NewClient([]Provider{
		NewOpenAIProvider(OpenAIOptions{Name: "p1", BaseURL: p1.srv.URL, APIKey: "k", Model: "m"}),
		NewOpenAIProvider(OpenAIOptions{Name: "p2", BaseURL: p2.srv.URL, APIKey: "k", Model: "m"}),
	}, ClientOptions{})
	if _, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err == nil {
		t.Fatal("全失败应报错")
	}
}

func TestClientNoProvider(t *testing.T) {
	// 新语义：允许空链构造（provider watcher 热注入场景），Chat 时返回 ErrNoProvider。
	c, err := NewClient(nil, ClientOptions{})
	if err != nil {
		t.Fatalf("空链构造不应报错: %v", err)
	}
	if _, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("空链 Chat 应返回 ErrNoProvider, got %v", err)
	}
}

func TestClientSuccessNoFallback(t *testing.T) {
	primary := newFakeOpenAI(t, okHandler("primary-ok"))
	backup := newFakeOpenAI(t, okHandler("backup-ok"))
	c, _ := NewClient([]Provider{
		NewOpenAIProvider(OpenAIOptions{Name: "primary", BaseURL: primary.srv.URL, APIKey: "k", Model: "m"}),
		NewOpenAIProvider(OpenAIOptions{Name: "backup", BaseURL: backup.srv.URL, APIKey: "k", Model: "m"}),
	}, ClientOptions{})
	resp, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil || resp.Content != "primary-ok" {
		t.Fatalf("主成功不应走 fallback: %v %+v", err, resp)
	}
	if backup.calls.Load() != 0 {
		t.Error("主成功时 backup 不应被调用")
	}
}
