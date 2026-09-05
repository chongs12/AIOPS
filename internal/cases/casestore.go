// Package casestore 案例沉淀与相似召回（M5）。
// 已解决工单写入案例库，新工单秒级召回相似历史处置。
// 实现：Milvus（优先，向量检索）+ PG（兜底，内存 bigram 余弦）。
// 嵌入使用字符 bigram 哈希（64 维、确定性、中文友好）——生产可平滑替换为
// 真实 embedding 模型，接口不变。
package cases

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"
)

// Case 已解决工单案例。
type Case struct {
	TicketID  string    `json:"ticket_id"`
	Type      string    `json:"type"`
	Service   string    `json:"service"`
	RootCause string    `json:"root_cause"`
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

// Scored 带相似度得分的案例。
type Scored struct {
	Case
	Score float64 `json:"score"`
}

// Store 案例库接口。
type Store interface {
	Save(ctx context.Context, c *Case) error
	Search(ctx context.Context, query string, topK int) ([]Scored, error)
	Backend() string
}

// TextOf 案例的可检索文本。
func TextOf(c *Case) string {
	return strings.Join([]string{c.Type, c.Service, c.RootCause, c.Summary}, "\n")
}

// Embed 字符 bigram 哈希到 64 维并 L2 归一化。
func Embed(text string) []float32 {
	const dim = 64
	v := make([]float32, dim)
	runes := []rune(strings.ToLower(text))
	for i := 0; i+1 < len(runes); i++ {
		bg := string(runes[i : i+2])
		h := fnv.New32a()
		_, _ = h.Write([]byte(bg))
		v[h.Sum32()%dim]++
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / norm)
		}
	}
	return v
}

// cosine 向量余弦（已归一化即点积）。
func cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// ---- PG 兜底实现 ----

// PGStore PG 存储 + 内存 bigram 余弦相似（近 500 条范围内检索）。
type PGStore struct {
	db *sql.DB
}

// NewPG 构造并迁移。
func NewPG(ctx context.Context, db *sql.DB) (*PGStore, error) {
	const schema = `
CREATE TABLE IF NOT EXISTS cases (
    id         BIGSERIAL PRIMARY KEY,
    ticket_id  TEXT NOT NULL,
    type       TEXT NOT NULL DEFAULT '',
    service    TEXT NOT NULL DEFAULT '',
    root_cause TEXT NOT NULL DEFAULT '',
    summary    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("cases 建表迁移: %w", err)
	}
	return &PGStore{db: db}, nil
}

// Backend 实现标识。
func (s *PGStore) Backend() string { return "postgres(fallback)" }

// Save 落库案例。
func (s *PGStore) Save(ctx context.Context, c *Case) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO cases (ticket_id, type, service, root_cause, summary, created_at)
VALUES ($1,$2,$3,$4,$5,COALESCE($6, now()))`,
		c.TicketID, c.Type, c.Service, c.RootCause, c.Summary, nullableTime(c.CreatedAt))
	return err
}

// Search 载入近 500 条做 bigram 余弦排序。
func (s *PGStore) Search(ctx context.Context, query string, topK int) ([]Scored, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ticket_id, type, service, root_cause, summary, created_at
FROM cases ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Scored
	qv := Embed(query)
	for rows.Next() {
		var c Case
		if err := rows.Scan(&c.TicketID, &c.Type, &c.Service, &c.RootCause, &c.Summary, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, Scored{Case: c, Score: cosine(qv, Embed(TextOf(&c)))})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortScored(out)
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func sortScored(s []Scored) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Score > s[j-1].Score; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
