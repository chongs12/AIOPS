package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// jsonSystemSuffix 追加到 system 消息的结构化输出约束。
const jsonSystemSuffix = "你的输出必须是单个合法 JSON 对象，不要输出任何解释、注释或 Markdown 代码围栏。"

// ChatJSON 结构化输出：强约束 JSON 输出 → 剥围栏/抽取 JSON → 解析到 out；
// 解析失败携带错误自动修复重试一次，仍失败则报错（含输出摘录）。
func (c *Client) ChatJSON(ctx context.Context, req ChatRequest, out any) error {
	messages := withJSONInstruction(req.Messages)
	base := req
	base.Messages = messages

	resp, err := c.Chat(ctx, base)
	if err != nil {
		return err
	}
	if err := decodeJSON(resp.Content, out); err == nil {
		return nil
	}

	// 修复重试：把上次的错误输出与解析错误回喂。
	repair := slices.Clone(messages)
	repair = append(repair,
		Message{Role: RoleAssistant, Content: resp.Content},
		Message{Role: RoleUser, Content: fmt.Sprintf(
			"你上一次的输出无法被解析为 JSON（错误：%v）。请重新输出，只输出合法 JSON 本体，不要任何解释或围栏。", err)},
	)
	base.Messages = repair
	resp2, err := c.Chat(ctx, base)
	if err != nil {
		return fmt.Errorf("结构化输出修复重试失败: %w", err)
	}
	if err := decodeJSON(resp2.Content, out); err != nil {
		return fmt.Errorf("结构化输出解析失败（已重试一次）: %w；末次输出: %s", err, excerpt(resp2.Content, 200))
	}
	return nil
}

// withJSONInstruction 保证消息里带有 JSON 输出约束（追加或融合进既有 system）。
func withJSONInstruction(messages []Message) []Message {
	out := slices.Clone(messages)
	if len(out) == 0 {
		return []Message{{Role: RoleSystem, Content: jsonSystemSuffix}}
	}
	if out[0].Role == RoleSystem {
		if !strings.Contains(out[0].Content, "JSON") {
			out[0] = Message{Role: RoleSystem, Content: out[0].Content + "\n\n" + jsonSystemSuffix}
		}
		return out
	}
	return append([]Message{{Role: RoleSystem, Content: jsonSystemSuffix}}, out...)
}

// decodeJSON 抽取并解析 JSON。
func decodeJSON(content string, out any) error {
	s := extractJSON(content)
	if s == "" {
		return errors.New("输出中未找到 JSON")
	}
	return json.Unmarshal([]byte(s), out)
}

// extractJSON 从可能带解释/围栏的输出中抽取 JSON 本体：
// 先剥 Markdown 围栏，再取首个 {/[ 到最后一个 }/] 的片段。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 剥离代码围栏：```json\n...\n``` 或 ```\n...\n```。
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	first := -1
	for _, r := range [...]byte{'{', '['} {
		if i := strings.IndexByte(s, r); i >= 0 && (first == -1 || i < first) {
			first = i
		}
	}
	if first < 0 {
		return ""
	}
	lastBrace := strings.LastIndexByte(s, '}')
	lastBracket := strings.LastIndexByte(s, ']')
	last := lastBrace
	if lastBracket > last {
		last = lastBracket
	}
	if last <= first {
		return ""
	}
	return s[first : last+1]
}
