package evidence

import "testing"

func TestBoardObservations(t *testing.T) {
	b := NewBoard()
	id1 := b.AddObservation(Observation{Source: "prometheus.query", Query: "up", Status: "ok", Summary: "16 up"})
	id2 := b.AddObservation(Observation{Source: "prometheus.query", Query: "5xx", Status: "failed", Summary: "查询失败"})

	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("观察 ID 生成异常: %q %q", id1, id2)
	}
	if len(b.Observations) != 2 {
		t.Fatalf("观察数 = %d", len(b.Observations))
	}
	o, ok := b.ObservationByID(id2)
	if !ok || o.Query != "5xx" {
		t.Fatalf("ObservationByID 异常: %v %v", o, ok)
	}
	if _, ok := b.ObservationByID("obs-none"); ok {
		t.Fatal("不存在的 ID 不应命中")
	}
	if b.Observations[0].CreatedAt.IsZero() {
		t.Error("CreatedAt 应自动填充")
	}
}

func TestBoardHypotheses(t *testing.T) {
	b := NewBoard()
	hid := b.AddHypothesis(Hypothesis{Statement: "pod 重启导致 5xx", EvidenceIDs: []string{"obs-1"}, Confidence: 0.7})
	h := b.Hypotheses[0]
	if h.ID != hid || h.Status != HypothesisPending {
		t.Errorf("假设默认状态异常: %+v", h)
	}
}

func TestRenderForLLM(t *testing.T) {
	b := NewBoard()
	oid := b.AddObservation(Observation{Source: "prometheus.query", Query: "up", Status: "ok", Summary: "全部正常"})
	b.AddHypothesis(Hypothesis{Statement: "假设A", Confidence: 0.5})

	out := b.RenderForLLM()
	for _, want := range []string{oid, "全部正常", "假设A", "观察集", "假设集"} {
		if !contains(out, want) {
			t.Errorf("渲染缺 %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
