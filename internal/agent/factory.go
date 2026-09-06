// Package agent 工厂与装配。
package agent

import (
	"aiops/internal/tool/promtool"
	"aiops/pkg/llm"
)

// Deps Agent 构建依赖。
type Deps struct {
	LLM       *llm.Client
	Prompts   llm.TemplateStore // nil = 内置
	Templates *promtool.Lib     // nil = 内置
	PodNamer  PodNamer          // 服务名→Pod 前缀（nil = 原样）；由 config.K8s.PodPrefixFmt 派生
	Services  []string          // 平台服务清单（nil = 内置默认）；由 config.K8s.Services 注入
}

// NewAll 构建全部业务 Agent（工厂入口，业务侧只依赖 Agent 接口）。
func NewAll(d Deps) map[string]Agent {
	if d.Prompts == nil {
		d.Prompts = NewBuiltinPromptStore()
	}
	if d.Templates == nil {
		d.Templates = promtool.BuiltinLib()
	}
	if d.PodNamer == nil {
		d.PodNamer = func(service string) string { return service }
	}
	if len(d.Services) > 0 {
		KnownServices = d.Services
	}
	return map[string]Agent{
		NameIntent:    NewIntentAgent(),
		NameMetrics:   NewMetricsAgent(),
		NameLog:       NewLogAgent(d.PodNamer),
		NameRootCause: NewRootCauseAgent(d.Templates),
		NameReply:     NewReplyAgent(),
	}
}
