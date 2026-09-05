package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRewriteLocalhost 验证 *.localhost 与 localhost 改写、其余地址不改写。
func TestRewriteLocalhost(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"prometheus.localhost:8080", "127.0.0.1:8080", true},
		{"jaeger.localhost:8080", "127.0.0.1:8080", true},
		{"localhost:9090", "127.0.0.1:9090", true},
		{"PROMETHEUS.LOCALHOST:8080", "127.0.0.1:8080", true},
		{"example.com:80", "", false},
		{"127.0.0.1:8080", "", false},
		{"bad-addr", "", false},
	}
	for _, c := range cases {
		got, ok := rewriteLocalhost(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("rewriteLocalhost(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestNewClientPreservesHostHeader 验证经 *.localhost URL 访问时 Host 头原样保留。
func TestNewClientPreservesHostHeader(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(2 * time.Second)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if gotHost == "" {
		t.Error("Host 头不应为空")
	}
}
