package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTemp 写临时配置文件并注册清理。
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	path := writeTemp(t, "{}")
	cfg, missing, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":8090" {
		t.Errorf("Server.Addr 默认值 = %q, want :8090", cfg.Server.Addr)
	}
	if cfg.Prometheus.BaseURL != "http://prometheus.localhost:8080" {
		t.Errorf("Prometheus.BaseURL 默认值 = %q", cfg.Prometheus.BaseURL)
	}
	if len(missing) != 0 {
		t.Errorf("空配置不应有缺失占位符，got %v", missing)
	}
}

func TestLoadPlaceholder(t *testing.T) {
	t.Setenv("PG_PASSWORD", "s3cret")
	path := writeTemp(t, `
postgres:
  dsn: "postgres://aiops:${PG_PASSWORD}@localhost:5433/aiops"
`)
	cfg, missing, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "postgres://aiops:s3cret@localhost:5433/aiops"
	if cfg.Postgres.DSN != want {
		t.Errorf("DSN = %q, want %q", cfg.Postgres.DSN, want)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
}

func TestLoadPlaceholderMissing(t *testing.T) {
	path := writeTemp(t, `
llm:
  providers:
    - name: p1
      api_key: "${LLM_API_KEY}"
`)
	_, missing, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(missing) != 1 || missing[0] != "LLM_API_KEY" {
		t.Errorf("missing = %v, want [LLM_API_KEY]", missing)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("AIOPS_PROMETHEUS_BASE_URL", "http://localhost:30090")
	t.Setenv("AIOPS_TELEMETRY_ENABLED", "false")
	t.Setenv("AIOPS_REDIS_DB", "3")
	t.Setenv("AIOPS_CACHE_L1_SIZE", "64")
	path := writeTemp(t, "{}")
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Prometheus.BaseURL != "http://localhost:30090" {
		t.Errorf("覆盖后 Prometheus.BaseURL = %q", cfg.Prometheus.BaseURL)
	}
	if cfg.Telemetry.Enabled {
		t.Error("覆盖后 Telemetry.Enabled 应为 false")
	}
	if cfg.Redis.DB != 3 {
		t.Errorf("覆盖后 Redis.DB = %d, want 3", cfg.Redis.DB)
	}
	if cfg.Cache.L1Size != 64 {
		t.Errorf("覆盖后 Cache.L1Size = %d, want 64", cfg.Cache.L1Size)
	}
}

func TestEnvOverrideInvalid(t *testing.T) {
	t.Setenv("AIOPS_REDIS_DB", "not-a-number")
	path := writeTemp(t, "{}")
	if _, _, err := Load(path); err == nil {
		t.Error("非法数值应返回错误")
	}
}

func TestLoadFileMissing(t *testing.T) {
	if _, _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("文件不存在应返回错误")
	}
}
