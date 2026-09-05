package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Auditor 工具调用审计接口（变更类工具必须审计）。
type Auditor interface {
	Record(ctx context.Context, entry *AuditEntry) error
}

// AuditEntry 审计条目。
type AuditEntry struct {
	TicketID   string          `json:"ticket_id"`
	Tool       string          `json:"tool"`
	Input      json.RawMessage `json:"input,omitempty"`
	Status     string          `json:"status"`
	DurationMS int64           `json:"duration_ms"`
	Error      string          `json:"error,omitempty"`
	At         time.Time       `json:"at"`
}

// NoopAuditor 空审计。
type NoopAuditor struct{}

// Record 空实现。
func (NoopAuditor) Record(context.Context, *AuditEntry) error { return nil }

// validateInput 最小 JSON Schema 校验：顶层对象 + required 存在性 + 基本类型匹配。
// 支持的 type：string/number/boolean/object/array；未声明的参数放行。
func validateInput(s Schema, input json.RawMessage) error {
	if len(s.Input) == 0 {
		return nil
	}
	var spec struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(s.Input, &spec); err != nil {
		return fmt.Errorf("schema 解析失败: %w", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(input, &obj); err != nil {
		return fmt.Errorf("输入须为 JSON 对象: %w", err)
	}
	for _, r := range spec.Required {
		if _, ok := obj[r]; !ok {
			return fmt.Errorf("缺少必填参数 %q", r)
		}
	}
	for k, raw := range obj {
		p, ok := spec.Properties[k]
		if !ok || p.Type == "" {
			continue
		}
		if err := checkType(k, p.Type, raw); err != nil {
			return err
		}
	}
	return nil
}

func checkType(key, want string, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("参数 %q 解析失败: %w", key, err)
	}
	ok := false
	switch want {
	case "string":
		_, ok = v.(string)
	case "number":
		_, ok = v.(float64)
	case "boolean":
		_, ok = v.(bool)
	case "object":
		_, ok = v.(map[string]any)
	case "array":
		_, ok = v.([]any)
	default:
		ok = true // 未知类型放行
	}
	if !ok {
		return fmt.Errorf("参数 %q 类型应为 %s", key, want)
	}
	return nil
}

// inputHash 输入的稳定哈希（缓存 key 用）。
func inputHash(input json.RawMessage) string {
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:8])
}

// FormatInput 生成供日志/证据引用的紧凑输入摘要。
func FormatInput(input json.RawMessage) string {
	s := strings.TrimSpace(string(input))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
