package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestRender(t *testing.T) {
	got, err := Render("你好 {{name}}，当前服务 {{ service }}", map[string]string{"name": "SRE", "service": "gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "你好 SRE，当前服务 gateway" {
		t.Errorf("Render = %q", got)
	}
}

func TestRenderMissingVar(t *testing.T) {
	if _, err := Render("{{a}} {{b}}", map[string]string{"a": "1"}); err == nil {
		t.Fatal("缺变量应报错")
	}
}

func TestBuiltinStore(t *testing.T) {
	s := NewBuiltinStore(map[string]string{"greet": "hello {{who}}"})
	if _, err := RenderNamed(s, context.Background(), "greet", map[string]string{"who": "ops"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "nope"); err == nil {
		t.Fatal("不存在的模板应报错")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"好的，结果如下：\n{\"a\":1}\n希望有帮助", `{"a":1}`},
		{"前缀 [1,2,3] 后缀", "[1,2,3]"},
		{"完全没有结构化内容", ""},
		{"{", ""},
	}
	for _, c := range cases {
		if got := extractJSON(c.in); got != c.want {
			t.Errorf("extractJSON(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// chatServerOf 便捷构造单后端 Client。
func chatServerOf(t *testing.T, reply string) *Client {
	f := newFakeOpenAI(t, okHandler(reply))
	c, err := NewClient([]Provider{
		NewOpenAIProvider(OpenAIOptions{Name: "t", BaseURL: f.srv.URL, APIKey: "k", Model: "m"}),
	}, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChatJSONDirect(t *testing.T) {
	c := chatServerOf(t, `{"verdict":"ok","confidence":0.9}`)
	var out struct {
		Verdict    string  `json:"verdict"`
		Confidence float64 `json:"confidence"`
	}
	if err := c.ChatJSON(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "判定"}}}, &out); err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if out.Verdict != "ok" || out.Confidence != 0.9 {
		t.Errorf("out = %+v", out)
	}
}

func TestChatJSONFenceAndNoise(t *testing.T) {
	c := chatServerOf(t, "```json\n{\"a\":42}\n```")
	var out struct {
		A int `json:"a"`
	}
	if err := c.ChatJSON(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}}, &out); err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if out.A != 42 {
		t.Errorf("out = %+v", out)
	}
}

func TestChatJSONRepairRetry(t *testing.T) {
	// 首次返回坏 JSON，修复重试返回好 JSON。
	var callCount int
	f := newFakeOpenAI(t, func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		reply := "不是 JSON"
		if callCount >= 2 {
			reply = `{"ok":true}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
		})
	})
	c, err := NewClient([]Provider{
		NewOpenAIProvider(OpenAIOptions{Name: "t", BaseURL: f.srv.URL, APIKey: "k", Model: "m"}),
	}, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.ChatJSON(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}}, &out); err != nil {
		t.Fatalf("修复重试应成功: %v", err)
	}
	if !out.OK {
		t.Errorf("out = %+v", out)
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2", callCount)
	}
}

func TestChatJSONFailAfterRepair(t *testing.T) {
	c := chatServerOf(t, "永远不是 JSON")
	var out struct {
		A int `json:"a"`
	}
	if err := c.ChatJSON(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}}, &out); err == nil {
		t.Fatal("修复重试后仍失败应报错")
	}
}

func TestWithJSONInstruction(t *testing.T) {
	// 无 system → 注入。
	got := withJSONInstruction([]Message{{Role: RoleUser, Content: "u"}})
	if len(got) != 2 || got[0].Role != RoleSystem {
		t.Errorf("注入失败: %+v", got)
	}
	// 已有 system 且无 JSON 字样 → 追加。
	got = withJSONInstruction([]Message{{Role: RoleSystem, Content: "你是助理"}, {Role: RoleUser, Content: "u"}})
	if len(got) != 2 || got[0].Content == "你是助理" {
		t.Errorf("追加失败: %+v", got)
	}
	// 已含 JSON → 不重复追加。
	got = withJSONInstruction([]Message{{Role: RoleSystem, Content: "输出 JSON"}, {Role: RoleUser, Content: "u"}})
	if got[0].Content != "输出 JSON" {
		t.Errorf("重复追加: %+v", got)
	}
}
