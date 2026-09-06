// Package config 提供 YAML 配置加载：${VAR} 环境占位解析 + AIOPS_ 前缀环境变量覆盖。
// 约定：仓库内配置零明文凭证，敏感值一律用 ${VAR} 占位，启动时从环境注入。
package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const envPrefix = "AIOPS_"

// Config 全量配置。字段 env 标签用于环境变量覆盖，
// 如 Server.Addr 的覆盖变量为 AIOPS_SERVER_ADDR。
type Config struct {
	Server     Server       `yaml:"server" env:"SERVER"`
	Telemetry  Telemetry    `yaml:"telemetry" env:"TELEMETRY"`
	Prometheus Prometheus   `yaml:"prometheus" env:"PROMETHEUS"`
	Jaeger     Jaeger       `yaml:"jaeger" env:"JAEGER"`
	K8s        K8s          `yaml:"k8s" env:"K8S"`
	Postgres   Postgres     `yaml:"postgres" env:"POSTGRES"`
	Redis      Redis        `yaml:"redis" env:"REDIS"`
	Cache      Cache        `yaml:"cache" env:"CACHE"`
	RateLimit  RateLimit    `yaml:"ratelimit" env:"RATELIMIT"`
	LLM        LLM          `yaml:"llm" env:"LLM"`
	CaseStore  CaseStoreCfg `yaml:"case_store" env:"CASESTORE"`
	SlowQuery  SlowQueryCfg `yaml:"slow_query" env:"SLOWQUERY"`
	Auth       AuthCfg      `yaml:"auth" env:"AUTH"`
}

// AuthCfg 鉴权配置：默认关闭，启用后管理面/写操作强制 JWT。
type AuthCfg struct {
	Enabled   bool   `yaml:"enabled" env:"ENABLED"`
	JWTSecret string `yaml:"jwt_secret" env:"JWT_SECRET"`
	TTLHours  int    `yaml:"ttl_hours" env:"TTL_HOURS"`
}

// SlowQueryCfg 慢查询检测配置。
type SlowQueryCfg struct {
	ThresholdSeconds int `yaml:"threshold_seconds" env:"THRESHOLD_SECONDS"`
}

// CaseStoreCfg 案例库配置（Milvus 优先，不可达降级 PG）。
type CaseStoreCfg struct {
	MilvusAddr string `yaml:"milvus_addr" env:"MILVUS_ADDR"`
}

// Server HTTP 服务监听地址。
type Server struct {
	Addr        string `yaml:"addr" env:"ADDR"`
	MetricsAddr string `yaml:"metrics_addr" env:"METRICS_ADDR"`
}

// Telemetry OTel 配置；OTLPEndpoint 指向 Jaeger 的 OTLP gRPC 入口
// （宿主机开发模式需 make pf-otel，集群内部署时为 collector:4317）。
type Telemetry struct {
	Enabled           bool    `yaml:"enabled" env:"ENABLED"`
	ServiceName       string  `yaml:"service_name" env:"SERVICE_NAME"`
	OTLPEndpoint      string  `yaml:"otlp_endpoint" env:"OTLP_ENDPOINT"`
	SampleRatio       float64 `yaml:"sample_ratio" env:"SAMPLE_RATIO"`
	PortForwardTarget string  `yaml:"port_forward_target" env:"PORT_FORWARD_TARGET"`
}

// Prometheus 指标查询数据源（PreHire 集群 ingress）。
type Prometheus struct {
	BaseURL        string `yaml:"base_url" env:"BASE_URL"`
	TimeoutSeconds int    `yaml:"timeout_seconds" env:"TIMEOUT_SECONDS"`
}

// Jaeger 链路查询数据源（PreHire 集群 ingress）。
type Jaeger struct {
	BaseURL        string `yaml:"base_url" env:"BASE_URL"`
	TimeoutSeconds int    `yaml:"timeout_seconds" env:"TIMEOUT_SECONDS"`
}

// K8s 集群访问；服务清单/前缀可配置，避免 PreHire 命名约定硬编码进业务代码。
type K8s struct {
	Kubeconfig     string   `yaml:"kubeconfig" env:"KUBECONFIG"`
	Namespace      string   `yaml:"namespace" env:"NAMESPACE"`
	TimeoutSeconds int      `yaml:"timeout_seconds" env:"TIMEOUT_SECONDS"`
	Services       []string `yaml:"services,omitempty"`       // 平台服务清单（意图识别/前端下拉）
	PodPrefixFmt   string   `yaml:"pod_prefix_fmt,omitempty"` // 服务名→Pod 前缀格式，缺省 "%s"
	JobPrefix      string   `yaml:"job_prefix,omitempty"`     // Prometheus job 前缀，如 "makejob-"
}

// Postgres 工单/状态/证据存储。
type Postgres struct {
	DSN         string `yaml:"dsn" env:"DSN"`
	MaxOpenConn int    `yaml:"max_open_conn" env:"MAX_OPEN_CONN"`
}

// Redis 缓存/限流后端；故障时各使用方按自身降级语义处理。
type Redis struct {
	Addr          string `yaml:"addr" env:"ADDR"`
	Password      string `yaml:"password" env:"PASSWORD"`
	DB            int    `yaml:"db" env:"DB"`
	PoolSize      int    `yaml:"pool_size" env:"POOL_SIZE"`
	DialTimeoutMS int    `yaml:"dial_timeout_ms" env:"DIAL_TIMEOUT_MS"`
	ReadTimeoutMS int    `yaml:"read_timeout_ms" env:"READ_TIMEOUT_MS"`
}

// Cache 两级缓存参数；GetOrLoad 可按次覆盖 TTL。
type Cache struct {
	L1Size             int `yaml:"l1_size" env:"L1_SIZE"`
	L1TTLSeconds       int `yaml:"l1_ttl_seconds" env:"L1_TTL_SECONDS"`
	L2TTLSeconds       int `yaml:"l2_ttl_seconds" env:"L2_TTL_SECONDS"`
	NegativeTTLSeconds int `yaml:"negative_ttl_seconds" env:"NEGATIVE_TTL_SECONDS"`
}

// RateLimit 限流参数；RedisTimeoutMS 为限流判定的总超时（限流绝不比业务慢）。
type RateLimit struct {
	RedisTimeoutMS int `yaml:"redis_timeout_ms" env:"REDIS_TIMEOUT_MS"`
}

// LLM provider 列表：首个为主，其余构成 fallback 链。
type LLM struct {
	Providers             []LLMProvider `yaml:"providers"`
	RequestTimeoutSeconds int           `yaml:"request_timeout_seconds" env:"REQUEST_TIMEOUT_SECONDS"`
}

// LLMProvider 单个 OpenAI 兼容端点配置；APIKey 用 ${VAR} 占位。
type LLMProvider struct {
	Name           string  `yaml:"name"`
	BaseURL        string  `yaml:"base_url"`
	APIKey         string  `yaml:"api_key"`
	Model          string  `yaml:"model"`
	TimeoutSeconds int     `yaml:"timeout_seconds"`
	MaxTokens      int     `yaml:"max_tokens"`
	Temperature    float64 `yaml:"temperature"`
}

// Default 返回内置默认值；Load 时 YAML 中缺省的字段保留这些默认。
func Default() *Config {
	return &Config{
		Server: Server{Addr: ":8090", MetricsAddr: ":6060"},
		Telemetry: Telemetry{
			Enabled:           true,
			ServiceName:       "aiops",
			OTLPEndpoint:      "localhost:4317", // dev 模式经 make pf-otel 转发到集群内 Jaeger
			SampleRatio:       1.0,
			PortForwardTarget: "svc/makejob-jaeger",
		},
		Prometheus: Prometheus{BaseURL: "http://prometheus.localhost:8080", TimeoutSeconds: 10},
		Jaeger:     Jaeger{BaseURL: "http://jaeger.localhost:8080", TimeoutSeconds: 8},
		K8s: K8s{
			Namespace:      "default",
			TimeoutSeconds: 5,
			PodPrefixFmt:   "%s", // 缺省原样；PreHire 集群在 yaml 里配 "makejob-%s"
			JobPrefix:      "makejob-",
		},
		Postgres: Postgres{
			DSN:         "postgres://aiops:${PG_PASSWORD}@localhost:5433/aiops?sslmode=disable",
			MaxOpenConn: 20,
		},
		Redis: Redis{
			Addr:          "localhost:6380", // 专用实例，避免与宿主机既有 Redis 冲突
			Password:      "aiops_dev",      // 与 dev compose requirepass 默认值一致；生产经环境覆盖
			PoolSize:      10,
			DialTimeoutMS: 200,
			ReadTimeoutMS: 200,
		},
		Cache:     Cache{L1Size: 1024, L1TTLSeconds: 30, L2TTLSeconds: 300, NegativeTTLSeconds: 30},
		RateLimit: RateLimit{RedisTimeoutMS: 100},
		CaseStore: CaseStoreCfg{MilvusAddr: "localhost:19530"},
		SlowQuery: SlowQueryCfg{ThresholdSeconds: 1},
		Auth:      AuthCfg{Enabled: false, JWTSecret: "${JWT_SECRET}", TTLHours: 24},
		LLM: LLM{
			Providers: []LLMProvider{{
				Name:           "glm",
				BaseURL:        "https://open.bigmodel.cn/api/paas/v4",
				APIKey:         "${LLM_API_KEY}",
				Model:          "glm-4.5",
				TimeoutSeconds: 60,
				MaxTokens:      4096,
				Temperature:    0.2,
			}},
			RequestTimeoutSeconds: 90,
		},
	}
}

var placeholderRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load 读取 YAML 配置：占位符解析在结构体层统一进行（覆盖 YAML 值与
// Go 代码默认值两条来源），随后应用 AIOPS_ 前缀环境变量覆盖。
// 返回的 missing 为环境中缺失的占位变量名（不阻塞启动，由调用方决定是否告警）。
func Load(path string) (*Config, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, nil, fmt.Errorf("解析 YAML: %w", err)
	}
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, nil, fmt.Errorf("环境变量覆盖: %w", err)
	}

	var missing []string
	resolveStructPlaceholders(reflect.ValueOf(cfg).Elem(), &missing)
	return cfg, missing, nil
}

// resolveStructPlaceholders 递归解析结构体内所有字符串字段的 ${VAR} 占位。
// 缺失的环境变量置空并记录（切片内的结构体同样处理，覆盖 LLM providers 等）。
func resolveStructPlaceholders(v reflect.Value, missing *[]string) {
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).Tag.Get("yaml") == "-" {
				continue
			}
			resolveStructPlaceholders(v.Field(i), missing)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			resolveStructPlaceholders(v.Index(i), missing)
		}
	case reflect.String:
		s := v.String()
		if strings.Contains(s, "${") {
			resolved := placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
				name := m[2 : len(m)-1]
				if val, ok := os.LookupEnv(name); ok {
					return val
				}
				*missing = append(*missing, name)
				return ""
			})
			v.SetString(resolved)
		}
	}
}

// applyEnvOverrides 递归遍历结构体，对带 env 标签的叶字段应用 AIOPS_ 前缀覆盖。
// 切片字段不支持环境覆盖（敏感项应使用 ${VAR} 占位）。
func applyEnvOverrides(cfg *Config) error {
	return walkOverrides(reflect.ValueOf(cfg).Elem(), "")
}

func walkOverrides(v reflect.Value, prefix string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("env")
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + tag
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			if err := walkOverrides(fv, name+"_"); err != nil {
				return err
			}
		case reflect.Slice:
			continue
		default:
			envName := envPrefix + strings.ToUpper(name)
			raw, ok := os.LookupEnv(envName)
			if !ok {
				continue
			}
			if err := setScalar(fv, raw, envName); err != nil {
				return err
			}
		}
	}
	return nil
}

// setScalar 将字符串环境变量值写入标量字段。
func setScalar(fv reflect.Value, raw, envName string) error {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", envName, err)
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %w", envName, err)
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %w", envName, err)
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		x, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s: %w", envName, err)
		}
		fv.SetFloat(x)
	default:
		return fmt.Errorf("%s: 不支持覆盖类型 %s", envName, fv.Kind())
	}
	return nil
}
