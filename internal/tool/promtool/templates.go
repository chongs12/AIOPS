package promtool

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"aiops/pkg/llm"
)

// Param 模板参数声明。
type Param struct {
	Name     string `json:"name"`
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
	Desc     string `json:"desc,omitempty"`
	Pattern  string `json:"pattern,omitempty"` // 正则约束，防 PromQL 注入
}

// Template 查询模板：Expr 内用 {{param}} 占位。
type Template struct {
	Name   string  `json:"name"`
	Desc   string  `json:"desc"`
	Expr   string  `json:"expr"`
	Params []Param `json:"params,omitempty"`
}

// Lib 模板库。
type Lib struct {
	templates map[string]Template
}

// NewLib 构造模板库。
func NewLib(templates []Template) *Lib {
	m := make(map[string]Template, len(templates))
	for _, t := range templates {
		m[t.Name] = t
	}
	return &Lib{templates: m}
}

// Get 按名取模板。
func (l *Lib) Get(name string) (Template, bool) {
	t, ok := l.templates[name]
	return t, ok
}

// List 全部模板（按名排序，供 LLM 选择与展示）。
func (l *Lib) List() []Template {
	out := make([]Template, 0, len(l.templates))
	for _, t := range l.templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RenderForLLM 渲染模板清单（供意图/指标 Agent 的 prompt 使用）。
func (l *Lib) RenderForLLM() string {
	var sb strings.Builder
	for _, t := range l.List() {
		sb.WriteString(fmt.Sprintf("- %s: %s\n  表达式: %s\n", t.Name, t.Desc, t.Expr))
		if len(t.Params) > 0 {
			var ps []string
			for _, p := range t.Params {
				ps = append(ps, fmt.Sprintf("%s(%s)", p.Name, p.Desc))
			}
			sb.WriteString("  参数: " + strings.Join(ps, ", ") + "\n")
		}
	}
	return sb.String()
}

// paramRe 参数名合法性。
var paramRe = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// Render 校验参数并渲染模板为最终 PromQL：
// 缺省参数用默认值；required 缺失或 pattern 不匹配报错。
func (l *Lib) Render(t Template, params map[string]string) (string, error) {
	filled := make(map[string]string, len(t.Params))
	for _, p := range t.Params {
		v, ok := params[p.Name]
		if !ok || v == "" {
			if p.Required && p.Default == "" {
				return "", fmt.Errorf("模板 %s 缺少必填参数 %s", t.Name, p.Name)
			}
			v = p.Default
		}
		if !paramRe.MatchString(p.Name) {
			return "", fmt.Errorf("参数名非法: %s", p.Name)
		}
		if p.Pattern != "" {
			re, err := regexp.Compile("^" + p.Pattern + "$")
			if err != nil {
				return "", fmt.Errorf("参数 %s 的 pattern 非法: %w", p.Name, err)
			}
			if !re.MatchString(v) {
				return "", fmt.Errorf("参数 %s=%q 不匹配约束 %s", p.Name, v, p.Pattern)
			}
		}
		filled[p.Name] = v
	}
	// 未声明的参数直接丢弃（LLM 常惯性多传 window 等）；表达式只含已声明占位，
	// 丢弃不产生注入面；声明参数的值已由 pattern 校验。
	return llm.Render(t.Expr, filled)
}

// BuiltinTemplates 内置查询模板（面向 PreHire 集群真实指标，见 docs/03-data-source.md）。
// 后续场景（OOM/慢查询）在此追加。
func BuiltinTemplates() []Template {
	windowParam := Param{Name: "window", Desc: "时间窗，如 5m/30m/1h", Default: "5m", Pattern: `[1-9][0-9]*(s|m|h|d)`}
	jobParam := Param{Name: "job", Desc: "Prometheus job 正则", Default: ".*", Pattern: `[^{}]{1,80}`}
	return []Template{
		{
			Name:   "up_status",
			Desc:   "各抓取目标的存活状态（服务是否 up）",
			Expr:   `up{job=~"{{job}}"}`,
			Params: []Param{jobParam},
		},
		{
			Name:   "http_5xx_rate",
			Desc:   "5xx 请求速率（次/秒），定位错误量级",
			Expr:   `sum(rate(http_requests_total{code=~"5.."}[{{window}}]))`,
			Params: []Param{windowParam},
		},
		{
			Name:   "http_total_rate",
			Desc:   "全部请求速率（次/秒），用于计算错误占比",
			Expr:   `sum(rate(http_requests_total[{{window}}]))`,
			Params: []Param{windowParam},
		},
		{
			Name:   "http_5xx_ratio",
			Desc:   "5xx 占全部请求比例（0-1），判断故障严重度",
			Expr:   `sum(rate(http_requests_total{code=~"5.."}[{{window}}])) / clamp_min(sum(rate(http_requests_total[{{window}}])), 1e-9)`,
			Params: []Param{windowParam},
		},
		{
			Name:   "http_5xx_by_path",
			Desc:   "按路径+状态码聚合的 5xx Top5，定位具体出错接口",
			Expr:   `topk(5, sum by (path, code) (rate(http_requests_total{code=~"5.."}[{{window}}])))`,
			Params: []Param{windowParam},
		},
		{
			Name:   "http_latency_p99",
			Desc:   "接口 P99 延迟（秒），判断是否伴随性能劣化",
			Expr:   `histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket[{{window}}])) by (le))`,
			Params: []Param{windowParam},
		},
		{
			Name: "container_memory_working_set",
			Desc: "容器内存工作集 Top5（字节），排查内存压力/OOM 前兆",
			Expr: `topk(5, sum by (pod, container) (container_memory_working_set_bytes{container!=""}))`,
		},
	}
}

// BuiltinLib 内置模板库。
func BuiltinLib() *Lib { return NewLib(BuiltinTemplates()) }
