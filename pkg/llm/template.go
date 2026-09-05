package llm

import (
	"context"
	"fmt"
	"regexp"
)

// tplVarRe 模板变量占位：{{variable}}（允许两侧空白）。
var tplVarRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.]+)\s*\}\}`)

// Render 渲染 {{variable}} 占位模板。变量缺失时报错，防止静默生成残缺提示词。
func Render(tpl string, vars map[string]string) (string, error) {
	var missing []string
	out := tplVarRe.ReplaceAllStringFunc(tpl, func(m string) string {
		name := tplVarRe.FindStringSubmatch(m)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		missing = append(missing, name)
		return m
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("模板变量缺失: %v", missing)
	}
	return out, nil
}

// TemplateStore 模板来源。M0 提供内置实现；M4 接 DB 外置（两级缓存热更新），
// 接口不变。
type TemplateStore interface {
	Get(ctx context.Context, name string) (string, error)
}

// BuiltinStore 内置模板仓库。
type BuiltinStore struct {
	templates map[string]string
}

// NewBuiltinStore 用一组内置模板构造。
func NewBuiltinStore(templates map[string]string) *BuiltinStore {
	return &BuiltinStore{templates: templates}
}

// Get 按 name 取模板；未找到返回错误。
func (s *BuiltinStore) Get(_ context.Context, name string) (string, error) {
	tpl, ok := s.templates[name]
	if !ok {
		return "", fmt.Errorf("模板 %q 不存在", name)
	}
	return tpl, nil
}

// RenderNamed 取模板并渲染的便捷组合。
func RenderNamed(store TemplateStore, ctx context.Context, name string, vars map[string]string) (string, error) {
	tpl, err := store.Get(ctx, name)
	if err != nil {
		return "", err
	}
	return Render(tpl, vars)
}
