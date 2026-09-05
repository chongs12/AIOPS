package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// marshalCompact 紧凑 JSON（prompt 内嵌用）。
func marshalCompact(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("JSON 序列化失败: %w", err)
	}
	return string(b), nil
}

// nowUTC 当前 UTC 时间。
func nowUTC() time.Time { return time.Now().UTC() }

// parseTimeRFC3339 解析 RFC3339 时间。
func parseTimeRFC3339(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, strings.TrimSpace(s))
}
