package orchestrator

import (
	"context"
	"encoding/json"
	"testing"

	"aiops/internal/agent"
	"aiops/internal/ticket"
	"aiops/internal/tool"
)

// TestRunVerifyTaskResolvedTicket 直接验证 RunVerifyTask 对 resolved 工单的行为。
func TestRunVerifyTaskResolvedTicket(t *testing.T) {
	llmc := newPromptAwareLLM(t, rcaOK)
	tools := &scriptTools{items: []*tool.Result{{
		Status:   tool.StatusOK,
		Data:     json.RawMessage(`[{"metric":{},"value":[1,"0.01"]}]`),
		Evidence: tool.EvidenceMeta{Source: "prometheus", Query: "5xx"},
	}}}
	store := ticket.NewMemStore()
	o := New(Options{Store: store, Agents: agent.NewAll(agent.Deps{LLM: llmc}), Tools: tools, LLM: llmc})

	tk := ticket.New("t", "c", "r")
	tk.Status = ticket.StatusResolved
	if err := store.CreateTicket(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"ticket_id": tk.ID,
		"cfg":       DefaultVerifyConfig(),
	})
	if err := o.RunVerifyTask(context.Background(), payload); err != nil {
		t.Fatalf("RunVerifyTask 对 resolved 工单应可用: %v", err)
	}
	got, _ := store.GetTicket(context.Background(), tk.ID)
	if got.Status != ticket.StatusResolved {
		t.Fatalf("复查通过应保持 resolved: %s", got.Status)
	}
	msgs, _ := store.ListMessages(context.Background(), tk.ID)
	if len(msgs) == 0 || !containsSub(msgs[len(msgs)-1].Content, "复查通过") {
		t.Fatalf("应有复查通过消息: %+v", msgs)
	}
}

func containsSub(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
