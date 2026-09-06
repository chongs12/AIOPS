// Package slowquery 提供慢查询检测工具（M5 慢查询工单类型的真实数据源）：
// 查询 PostgreSQL pg_stat_activity 中超过阈值的活跃查询。
// DSN 缺省指向 AIOps 自身 PG；可配置指向业务库（演示时可造 pg_sleep 慢查询）。
package slowquery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"aiops/internal/tool"
)

// Tool 慢查询列表工具。
type Tool struct {
	db        *sql.DB
	threshold time.Duration
}

// New 构造。
func New(db *sql.DB, thresholdSeconds int) *Tool {
	if thresholdSeconds <= 0 {
		thresholdSeconds = 1
	}
	return &Tool{db: db, threshold: time.Duration(thresholdSeconds) * time.Second}
}

// Name 工具名。
func (t *Tool) Name() string { return "slowquery.list" }

// Desc 工具描述。
func (t *Tool) Desc() string {
	return "查询 PostgreSQL 中当前超过阈值的活跃慢查询（pg_stat_activity），返回 pid/用户/时长/语句。"
}

// Schema 工具 schema。
func (t *Tool) Schema() tool.Schema {
	return tool.Schema{
		Input: json.RawMessage(`{
  "type": "object",
  "properties": {
    "threshold_seconds": {"type": "number", "description": "慢查询阈值秒数，缺省用配置值"}
  }
}`),
		Idempotent:    true,
		Timeout:       8 * time.Second,
		RatePerSecond: 2,
		RateBurst:     4,
		Concurrency:   2,
	}
}

// row 慢查询行。
type row struct {
	PID      int32  `json:"pid"`
	User     string `json:"user"`
	State    string `json:"state"`
	Duration string `json:"duration"`
	Query    string `json:"query"`
}

// Execute 执行检测。失败返回 failed 信封（证据链语义）。
func (t *Tool) Execute(ctx context.Context, input json.RawMessage) (*tool.Result, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var in struct {
		ThresholdSeconds int `json:"threshold_seconds"`
	}
	_ = json.Unmarshal(input, &in)
	threshold := t.threshold
	if in.ThresholdSeconds > 0 {
		threshold = time.Duration(in.ThresholdSeconds) * time.Second
	}

	rows, err := t.db.QueryContext(ctx, `
SELECT pid, COALESCE(usename,''), COALESCE(state,''),
       to_char(now()-query_start, 'MI"m"SS" s"'), left(regexp_replace(query, E'[\\n\\r]+', ' ', 'g'), 200)
FROM pg_stat_activity
WHERE state <> 'idle' AND pid <> pg_backend_pid()
  AND now()-query_start > make_interval(secs => $1)
ORDER BY now()-query_start DESC`, threshold.Seconds())
	if err != nil {
		return &tool.Result{
			Status:         tool.StatusFailed,
			DegradedReason: fmt.Sprintf("慢查询检测不可用: %v", err),
			Evidence:       tool.EvidenceMeta{Source: "postgres"},
		}, nil
	}
	defer rows.Close()

	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.PID, &r.User, &r.State, &r.Duration, &r.Query); err == nil {
			out = append(out, r)
		}
	}
	if err := rows.Err(); err != nil {
		return &tool.Result{
			Status:         tool.StatusFailed,
			DegradedReason: fmt.Sprintf("慢查询结果读取失败: %v", err),
			Evidence:       tool.EvidenceMeta{Source: "postgres"},
		}, nil
	}

	data, err := json.Marshal(map[string]any{
		"threshold_seconds": int(threshold.Seconds()),
		"count":             len(out),
		"queries":           out,
	})
	if err != nil {
		return failed(fmt.Sprintf("结果序列化失败: %v", err)), nil
	}
	return &tool.Result{
		Status: tool.StatusOK,
		Data:   data,
		Evidence: tool.EvidenceMeta{
			Source:  "postgres",
			Query:   "pg_stat_activity (duration > " + threshold.String() + ")",
			Excerpt: firstRows(data),
		},
	}, nil
}

func failed(reason string) *tool.Result {
	return &tool.Result{
		Status:         tool.StatusFailed,
		DegradedReason: reason,
		Evidence:       tool.EvidenceMeta{Source: "postgres"},
	}
}

// firstRows 取前 3 条作为摘录（整体合法 JSON）。
func firstRows(data json.RawMessage) json.RawMessage {
	var rows []row
	if err := json.Unmarshal(data, &struct {
		Queries *[]row `json:"queries"`
	}{Queries: &rows}); err != nil || len(rows) == 0 {
		return nil
	}
	if len(rows) > 3 {
		rows = rows[:3]
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return nil
	}
	return out
}
