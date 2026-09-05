// fakeopenai 是「演示模式」的本地假 OpenAI 兼容服务：
// 按 prompt 语义返回意图/根因/报告的结构化 JSON，并把黑板渲染文本中的
// 真实观察 ID 回填进证据引用——即推理脚本固定，但喂给它的是**真实 Prometheus 数据**，
// 产出的证据链全部来自真实集群查询。
// 用途：暂无 LLM_API_KEY 时演示 M1 全链路；配置真实 key 后行为自动替换为真实推理。
// 启动: go run ./scripts/fakeopenai -addr 127.0.0.1:18091
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
	"sync"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18091", "监听地址")
	flag.Parse()

	var mu sync.Mutex
	calls := 0

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		log.Printf("[fakeopenai] 第 %d 次调用", n)

		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		content := ""
		if len(body.Messages) > 0 {
			content = body.Messages[len(body.Messages)-1].Content
		}

		reply := `{}`
		switch {
		case strings.Contains(content, "意图识别专家"):
			reply = `{"type":"5xx_surge","service":"gateway","window_minutes":30,
				"keywords":["5xx"],"need_more_info":false,"reason":"工单描述明确指向接口 5xx 突增"}`
		case strings.Contains(content, "根因分析专家"):
			// 回填 prompt 观察集里的第一个真实观察 ID（防幻觉机制同样生效）。
			id := firstObsID(content)
			var extra string
			if strings.Contains(content, "up_status") || strings.Contains(content, "http_5xx_by_path") {
				extra = ""
			}
			_ = extra
			reply = `{"hypotheses":[
					{"statement":"网关上报 5xx 速率显著升高，且存在具体出错路径，指向上游服务故障而非网关自身","evidence_ids":["` + id + `"],"confidence":0.85,"status":"confirmed"},
					{"statement":"P99 延迟同步升高，故障伴随性能劣化","evidence_ids":["` + id + `"],"confidence":0.5,"status":"pending"}],
				"root_cause":"依据 Prometheus 真实指标：网关 5xx 速率与占比突增，结合 Top 出错路径定位到具体上游接口。请结合日志确认根因（演示模式下以指标证据为准）。",
				"confidence":0.85,
				"timeline":[{"event":"5xx 速率突增被指标捕获","source":"` + id + `"}],
				"recommendations":[{"action":"check_logs","target":"gateway / 上游服务","description":"按出错路径检查对应服务日志，确认根因","risk":"low"},
					{"action":"rollback","target":"出错路径对应服务","description":"若为近期变更引发，执行回滚","risk":"high"}],
				"missing_evidence":["容器日志未接入（M2 提供 client-go 日志检索）"],
				"need_more_queries":[]}`
		case strings.Contains(content, "事故报告撰写专家"):
			reply = `{"summary":"依据真实 Prometheus 指标：网关 5xx 速率与占比突增，并定位到具体出错路径。结论与证据见下（演示模式：日志证据待 M2 接入）。",
				"citations":[]}`
		default:
			reply = `{"summary":"ok"}`
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 100},
		})
	})

	log.Printf("[fakeopenai] 演示 LLM 启动于 http://%s/v1", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// firstObsID 从黑板渲染文本里抓取第一个 [obs-xxx]。
func firstObsID(s string) string {
	i := strings.Index(s, "[obs-")
	if i < 0 {
		return "obs-none"
	}
	j := strings.IndexByte(s[i:], ']')
	if j < 0 {
		return "obs-none"
	}
	return s[i+1 : i+j]
}
