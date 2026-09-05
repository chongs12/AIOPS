package ticket

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/evidence"
)

func TestMemStoreTicketLifecycle(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	tk := New("接口 5xx 突增", "gateway 大量 5xx", "oncall")
	if err := s.CreateTicket(ctx, tk); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTicket(ctx, tk.ID)
	if err != nil || got.Title != tk.Title {
		t.Fatalf("GetTicket: %v %+v", err, got)
	}

	got.Status = StatusResolved
	got.Report = &Report{RootCause: "pod 被删", Confidence: 0.9, GeneratedAt: time.Now()}
	if err := s.UpdateTicket(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetTicket(ctx, tk.ID)
	if got2.Status != StatusResolved || got2.Report == nil || got2.Report.Confidence != 0.9 {
		t.Fatalf("更新未生效: %+v", got2)
	}

	if _, err := s.GetTicket(ctx, "t-none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在应返回 ErrNotFound: %v", err)
	}
}

func TestMemStoreMessagesAndObservations(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	tk := New("t", "c", "r")
	_ = s.CreateTicket(ctx, tk)

	_ = s.AppendMessage(ctx, tk.ID, &Message{Role: "agent", Agent: "intent", Content: "已识别"})
	_ = s.AppendMessage(ctx, tk.ID, &Message{Role: "agent", Agent: "metrics", Content: "指标已查"})
	msgs, _ := s.ListMessages(ctx, tk.ID)
	if len(msgs) != 2 || msgs[0].Seq != 1 || msgs[1].Seq != 2 {
		t.Fatalf("消息序号异常: %+v", msgs)
	}

	oid := "obs-test"
	_ = s.SaveObservation(ctx, tk.ID, &evidence.Observation{ID: oid, Source: "prometheus.query", Status: "ok"})
	obs, _ := s.ListObservations(ctx, tk.ID)
	if len(obs) != 1 || obs[0].ID != oid {
		t.Fatalf("观察存取异常: %+v", obs)
	}
}

func TestNewIDFormat(t *testing.T) {
	id := NewID()
	if len(id) != len("t-20060102-150405-xxxx") {
		t.Errorf("工单号格式异常: %q", id)
	}
	if id[:2] != "t-" {
		t.Errorf("工单号前缀异常: %q", id)
	}
}

func TestEntitiesWindowEndOrNow(t *testing.T) {
	e := Entities{}
	if e.WindowEndOrNow().IsZero() {
		t.Error("缺省应返回 now")
	}
	future := time.Now().Add(time.Hour)
	e.WindowEnd = &future
	if !e.WindowEndOrNow().Equal(future) {
		t.Error("应返回显式窗口终点")
	}
}
