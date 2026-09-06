package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aiops/pkg/cache"
	"aiops/pkg/llm"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// TestApplySceneOverridesModel 验证 scene config 的 model 覆盖真实生效。
func TestApplySceneOverridesModel(t *testing.T) {
	// 最小可用的 store：不走 DB，直接构造一个 SceneConfigSource
	rc := &RunContext{
		SceneCfg: mapSceneCfg{"rootcause": {Model: "invalid-model-xyz", Temperature: 0.55}},
		Ctx:      context.Background(),
	}
	req := llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}}
	applyScene(rc, NameRootCause, &req)
	if req.Model != "invalid-model-xyz" {
		t.Fatalf("model 应被覆盖为 invalid-model-xyz, got %q", req.Model)
	}
	if req.Temperature != 0.55 {
		t.Fatalf("temperature 应被覆盖为 0.55, got %v", req.Temperature)
	}
}

// mapSceneCfg 测试桩。
type mapSceneCfg map[string]SceneParams

func (m mapSceneCfg) GetSceneConfig(_ context.Context, scene string) (SceneParams, bool) {
	sp, ok := m[scene]
	return sp, ok
}

var (
	_ = json.Marshal
	_ = time.Second
	_ = cache.Cache{}
	_ = singleflight.Group{}
	_ = redis.NewClient
	_ = llm.RoleUser
)
