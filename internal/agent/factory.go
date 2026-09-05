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
}

// NewAll 构建全部业务 Agent（工厂入口，业务侧只依赖 Agent 接口）。
func NewAll(d Deps) map[string]Agent {
	if d.Prompts == nil {
		d.Prompts = NewBuiltinPromptStore()
	}
	if d.Templates == nil {
		d.Templates = promtool.BuiltinLib()
	}
	return map[string]Agent{
		NameIntent:    NewIntentAgent(),
		NameMetrics:   NewMetricsAgent(),
		NameLog:       NewLogAgent(),
		NameRootCause: NewRootCauseAgent(d.Templates),
		NameReply:     NewReplyAgent(),
	}
}
