// Package promtool 提供 Prometheus 查询工具：客户端 + 查询模板库（D6 决策——
// LLM 只能选模板并填参，不能自由生成 PromQL，杜绝幻觉查询与高基数风险）。
package promtool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aiops/pkg/httpx"
)

// Client Prometheus HTTP API 客户端。
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient 构造客户端（内置 *.localhost 拨号映射）。
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), HTTP: httpx.NewClient(timeout)}
}

// queryResponse Prometheus /api/v1/query 响应（只取所需字段）。
type queryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// QueryInstant 执行即时查询（at 为零值表示 now）。
func (c *Client) QueryInstant(ctx context.Context, promql string, at time.Time) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("query", promql)
	if !at.IsZero() {
		q.Set("time", fmt.Sprintf("%d", at.Unix()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/query?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var qr queryResponse
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, fmt.Errorf("prometheus 响应解析失败: %w", err)
	}
	if qr.Status != "success" {
		return nil, fmt.Errorf("prometheus 查询错误: %s", qr.Error)
	}
	return qr.Data.Result, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
