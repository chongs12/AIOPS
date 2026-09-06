// Package api 推理回放：聚合工单消息/观察/审计/状态为按时间排序的事件流。
package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// replayEvent 回放事件（统一视图）。
type replayEvent struct {
	TS     time.Time `json:"ts"`
	Type   string    `json:"type"`  // message|observation|audit|status
	Actor  string    `json:"actor"` // user|agent:<name>|system|tool:<name>
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	RefID  string    `json:"ref_id,omitempty"` // obs-xxx / 消息 seq 等
}

// replayFeed 回放响应。
type replayFeed struct {
	TraceID  string        `json:"trace_id"`
	TraceURL string        `json:"trace_url"`
	Events   []replayEvent `json:"events"`
}

// ticketReplay 回放事件流（P3）。
func (s *Server) ticketReplay(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()
	tk, err := s.store.GetTicket(ctx, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	feed := replayFeed{TraceID: tk.TraceID, TraceURL: s.traceURL(tk.TraceID), Events: []replayEvent{}}

	// 消息事件（用户提问/Agent 叙述/系统）。
	if msgs, err := s.store.ListMessages(ctx, id); err == nil {
		for _, m := range msgs {
			actor, title := "system", "系统"
			switch m.Role {
			case "user":
				actor, title = "user", "提交人"
			case "agent":
				actor = "agent:" + m.Agent
				title = "Agent · " + m.Agent
			}
			feed.Events = append(feed.Events, replayEvent{
				TS: m.CreatedAt, Type: "message", Actor: actor,
				Title: title, Detail: m.Content, RefID: m.Agent,
			})
		}
	}

	// 观察事件（工具查询 = 证据产生时刻）。
	if obs, err := s.store.ListObservations(ctx, id); err == nil {
		for _, o := range obs {
			feed.Events = append(feed.Events, replayEvent{
				TS: o.CreatedAt, Type: "observation", Actor: "tool:" + o.Source,
				Title: "工具查询 · " + o.Source, Detail: o.Summary, RefID: o.ID,
			})
		}
	}

	// 审计事件（含处置动作）。
	if s.audit != nil {
		if entries, err := s.audit.ListByTicket(ctx, id); err == nil {
			for _, e := range entries {
				feed.Events = append(feed.Events, replayEvent{
					TS: e.At, Type: "audit", Actor: "tool:" + e.Tool,
					Title: "工具执行 · " + e.Tool, Detail: e.Error, RefID: e.Tool,
				})
			}
		}
	}

	// 稳定排序键：(时间, 类型, ref)——同一时刻的观察先于消息先于审计。
	sort.SliceStable(feed.Events, func(i, j int) bool {
		a, b := feed.Events[i], feed.Events[j]
		if !a.TS.Equal(b.TS) {
			return a.TS.Before(b.TS)
		}
		return typeOrder(a.Type) < typeOrder(b.Type)
	})

	c.JSON(http.StatusOK, feed)
}

func typeOrder(t string) int {
	switch t {
	case "observation":
		return 0
	case "message":
		return 1
	default:
		return 2
	}
}
