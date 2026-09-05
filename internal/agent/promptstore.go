package agent

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"aiops/pkg/cache"
	"aiops/pkg/llm"

	"github.com/redis/go-redis/v9"
)

// DBPromptStore DB 外置 Prompt 仓库（M4 管理面）：
// 两级缓存 L1 5s / L2 60s + 版本号失效——管理面改配置 5s 内生效、无需重启；
// DB 不可用或场景未配置时回退内置模板。
type DBPromptStore struct {
	db      *sql.DB
	cache   *cache.Cache
	ver     *cache.Versioner
	builtin llm.TemplateStore
}

// promptScenes 全部内置场景（管理面展示用）。
var promptScenes = []string{"intent", "rootcause", "reply"}

// NewDBPromptStore 构造并迁移；db 为 nil 或迁移失败返回错误（调用方降级内置）。
func NewDBPromptStore(ctx context.Context, db *sql.DB, rdb redis.UniversalClient) (*DBPromptStore, error) {
	if db == nil {
		return nil, fmt.Errorf("agent: 无 DB，prompt 外置不可用")
	}
	const schema = `
CREATE TABLE IF NOT EXISTS agent_configs (
    scene      TEXT PRIMARY KEY,
    prompt     TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("agent_configs 迁移: %w", err)
	}
	c := cache.New(cache.Options{L1Size: 64, L1TTL: 5 * time.Second, L2: rdb, NegativeTTL: 5 * time.Second})
	return &DBPromptStore{
		db:      db,
		cache:   c,
		ver:     cache.NewVersioner(rdb),
		builtin: NewBuiltinPromptStore(),
	}, nil
}

// Get 取场景模板：缓存 → DB → 内置。任何故障都回退内置，绝不阻塞主路径。
func (s *DBPromptStore) Get(ctx context.Context, scene string) (string, error) {
	key := s.ver.VersionedKey(ctx, "prompt", scene)
	v, _, err := s.cache.GetOrLoad(ctx, key, 60*time.Second, func(ctx context.Context) ([]byte, error) {
		var tpl string
		row := s.db.QueryRowContext(ctx, `SELECT prompt FROM agent_configs WHERE scene=$1`, scene)
		if err := row.Scan(&tpl); err == nil {
			return []byte(tpl), nil
		}
		// 未配置 → 内置（作为缓存值，哨兵语义即“内置”）
		b, err := s.builtin.Get(ctx, scene)
		if err != nil {
			return nil, err
		}
		return []byte(b), nil
	})
	if err != nil {
		return s.builtin.Get(ctx, scene) // 缓存/DB 故障 → 内置
	}
	return string(v), nil
}

// Set 管理面更新 prompt（立即失效版本号，≤5s 全量生效）。
func (s *DBPromptStore) Set(ctx context.Context, scene, prompt string) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO agent_configs (scene, prompt, updated_at) VALUES ($1,$2,now())
ON CONFLICT (scene) DO UPDATE SET prompt=EXCLUDED.prompt, updated_at=now()`, scene, prompt); err != nil {
		return err
	}
	_, err := s.ver.BumpVersion(ctx, "prompt")
	return err
}

// Reset 删除 DB 覆盖，恢复内置模板。
func (s *DBPromptStore) Reset(ctx context.Context, scene string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_configs WHERE scene=$1`, scene); err != nil {
		return err
	}
	_, err := s.ver.BumpVersion(ctx, "prompt")
	return err
}

// SceneInfo 管理面场景视图。
type SceneInfo struct {
	Scene     string `json:"scene"`
	Source    string `json:"source"` // db | builtin
	Prompt    string `json:"prompt"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ListScenes 列出全部场景及当前生效模板。
func (s *DBPromptStore) ListScenes(ctx context.Context) ([]SceneInfo, error) {
	out := []SceneInfo{}
	for _, scene := range promptScenes {
		var tpl string
		var updated sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT prompt, to_char(updated_at,'YYYY-MM-DD HH24:MI:SS') FROM agent_configs WHERE scene=$1`, scene).
			Scan(&tpl, &updated)
		if err == sql.ErrNoRows {
			b, err2 := s.builtin.Get(ctx, scene)
			if err2 != nil {
				continue
			}
			out = append(out, SceneInfo{Scene: scene, Source: "builtin", Prompt: b})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, SceneInfo{Scene: scene, Source: "db", Prompt: tpl, UpdatedAt: updated.String})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scene < out[j].Scene })
	return out, nil
}
