// Package llm 提供 LLM 调用能力（对标 PreHire app/ai_gateway 的工程模式）：
// OpenAI 兼容 provider 工厂 + fallback 链 + {{variable}} Prompt 模板 + 结构化输出。
// 全程内建自可观测：每次调用一个 span、ai_calls_total / ai_tokens_total /
// ai_call_duration_seconds 指标（命名对齐 PreHire）。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aiops/pkg/httpx"
)

// Role 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message 聊天消息。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// ChatRequest 一次对话请求；Temperature/MaxTokens/Model 为零值时使用 provider 默认。
// Model 支持按场景覆盖（管理面热更新），仅当 provider 侧配置同名/兼容模型时生效。
// Agent 为场景归因标签（intent/rootcause/reply...），进入 ai_* 指标维度。
type ChatRequest struct {
	Messages    []Message
	Temperature float64
	MaxTokens   int
	Model       string
	Agent       string
}

// ChatResponse 一次对话结果（含用量，供指标统计）。
type ChatResponse struct {
	Content          string
	Model            string
	Provider         string
	PromptTokens     int
	CompletionTokens int
}

// Provider 单个聊天补全后端。
type Provider interface {
	Name() string
	Model() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// OpenAIOptions OpenAI 兼容端点配置（智谱 GLM / DeepSeek / OpenAI 等均可）。
type OpenAIOptions struct {
	Name        string
	BaseURL     string // 如 https://open.bigmodel.cn/api/paas/v4
	APIKey      string
	Model       string
	Timeout     time.Duration // 单次请求超时，默认 60s
	MaxTokens   int
	Temperature float64
}

// NewOpenAIProvider 构造 OpenAI 兼容 provider。
func NewOpenAIProvider(opts OpenAIOptions) Provider {
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.Name == "" {
		opts.Name = opts.BaseURL
	}
	return &openaiProvider{opts: opts, httpc: httpx.NewClient(opts.Timeout)}
}

type openaiProvider struct {
	opts  OpenAIOptions
	httpc *http.Client
}

type chatPayload struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type chatCompletionResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *openaiProvider) Name() string { return p.opts.Name }

// Model 返回该 provider 配置的默认模型名（归因维度兜底）。
func (p *openaiProvider) Model() string { return p.opts.Model }

// Chat 调用 POST {BaseURL}/chat/completions。
func (p *openaiProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	temperature := p.opts.Temperature
	if req.Temperature > 0 {
		temperature = req.Temperature
	}
	maxTokens := p.opts.MaxTokens
	if req.MaxTokens > 0 {
		maxTokens = req.MaxTokens
	}

	model := p.opts.Model
	if req.Model != "" {
		model = req.Model
	}
	body, err := json.Marshal(chatPayload{
		Model: model, Messages: req.Messages,
		Temperature: temperature, MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(p.opts.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.opts.APIKey)

	resp, err := p.httpc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider %s 请求失败: %w", p.opts.Name, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider %s HTTP %d: %s", p.opts.Name, resp.StatusCode, excerpt(string(raw), 200))
	}

	var cr chatCompletionResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("provider %s 响应解析失败: %w", p.opts.Name, err)
	}
	if cr.Error != nil {
		return nil, fmt.Errorf("provider %s 返回错误: %s", p.opts.Name, cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("provider %s 无 choices", p.opts.Name)
	}
	return &ChatResponse{
		Content:          cr.Choices[0].Message.Content,
		Model:            p.opts.Model,
		Provider:         p.opts.Name,
		PromptTokens:     cr.Usage.PromptTokens,
		CompletionTokens: cr.Usage.CompletionTokens,
	}, nil
}

// excerpt 截取前 n 个字符。
func excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
