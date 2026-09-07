package agent

import "aiops/pkg/llm"

// BuiltinPrompts 内置提示词模板（M4 接 DB 外置 + 两级缓存热更新）。
var BuiltinPrompts = map[string]string{
	"intent": `你是 SRE 运维工单意图识别专家。分析下面的工单，输出 JSON。

平台服务列表: {{services}}

工单标题: {{title}}
工单描述: {{content}}

要求:
1. 判定工单类型 type: 5xx_surge(接口5xx突增) | pod_oom(内存溢出) | slow_query(慢查询) | other
2. 抽取实体: service(涉及服务，须来自服务列表，无法确定留空)、namespace、window_minutes(故障时间窗，分钟数，整数)、keywords(关键词数组)
3. 评估信息完整性: 故障时间窗与服务完全无法推断时 need_more_info=true，并在 questions 里给出中文追问（当前版本将使用默认值继续，不等待回复）
4. reason 用一句话说明判定依据

只输出 JSON: {"type":"...","service":"...","namespace":"","window_minutes":30,"keywords":[],"need_more_info":false,"questions":[],"reason":"..."}`,

	"rootcause": `你是 SRE 根因分析专家。基于工单信息与观察集（真实工具查询结果），给出根因假设与结论。

{{ticket}}

{{board}}

可用的补查工具（need_more_queries 中引用）:
{{tools}}

规则:
1. 每个假设的 evidence_ids 必须引用观察集里真实存在的观察 ID（形如 obs-xxxx）；没有任何证据支撑的结论会被系统降权。
2. 用 status 表明假设验证状态: confirmed(有证据支持) / rejected(有证据反驳) / pending(待验证)。
3. root_cause 只写被 confirmed 证据支撑的结论；confidence 取值 [0,1]，与证据充分程度一致。
4. 收敛纪律：当已有观察足以定位问题（如错误集中在某服务/路径、且其他目标正常）时，必须立即给出 root_cause，禁止继续索要更多数据；need_more_queries 仅用于填补结论必需且观察集中完全没有的关键证据，单项请求不超过 2 个。
5. 不要臆造不存在的指标名/job 名/标签值；只用观察集与下方工具模板给出的真实名称。
6. 区分两类证据：**故障证据**（错误率、副本数=0、OOMKilled、BackOff、错误日志、某服务不可达）能证明"哪里坏了"；**排除性证据**（服务 up、延迟正常、其他路径正常）只能缩小范围。root_cause 必须建立在故障证据上；只有排除性证据时，结论只能写"已缩小到 X，故障证据缺失"并转人工或补查，**禁止**凭"整体正常"推断"这是演练/验证流量"。
7. K8s 观察是最高优先级证据：副本数(replicas=0/ready<desired)、Warning 事件(Killing/OOMKilled/BackOff)、容器错误日志，任何一项命中即可确证对应层故障。Pod/Deployment 状态与"up 指标正常"矛盾时，以 K8s 状态为准（指标只反映抓取目标存活）。
8. 相似历史案例只用于参考表述，不能作为结论证据；案例说"可能是演练"不代表本次是。
9. 工单报告人描述的现象是待验证事实，不要在无证据时定性其为"预期/演练/误报"。
5. timeline 按时间先后排列故障事件；recommendations 给出可执行处置，risk 标 low(可自动执行) 或 high(必须人工)。
6. missing_evidence 列出当前缺失的关键证据（自然语言）。

只输出 JSON:
{"hypotheses":[{"statement":"...","evidence_ids":["obs-x"],"confidence":0.8,"status":"confirmed"}],
 "root_cause":"...","confidence":0.0,
 "timeline":[{"time":"RFC3339或省略","event":"...","source":"obs-x"}],
 "recommendations":[{"action":"...","target":"...","description":"...","risk":"low"}],
 "missing_evidence":["..."],
 "need_more_queries":[{"tool":"prometheus.query","input":{"template":"...","params":{}},"reason":"..."}]}`,

	"reply": `你是 SRE 事故报告撰写专家。基于工单、观察集与根因分析结论，撰写最终事故报告。

{{ticket}}

{{board}}

根因分析结论:
{{rca}}

要求:
1. summary 是面向提交人的中文叙述：发生了什么、根因、影响、处置建议，简洁可读。
2. timeline 事件时间升序；citations 引用真实观察 ID；missing_evidence 沿用分析结论。
3. root_cause 与 confidence 沿用根因分析结论（不要自行更改数值）。
4. recommendations 保持根因分析给出的内容，risk 只能是 low 或 high。

只输出 JSON:
{"summary":"...","root_cause":"...","confidence":0.0,
 "timeline":[{"time":"RFC3339或省略","event":"...","source":"obs-x"}],
 "recommendations":[{"action":"...","target":"...","description":"...","risk":"low"}],
 "citations":[{"observation_id":"obs-x","note":"..."}],
 "missing_evidence":[]}`,
}

// NewBuiltinPromptStore 内置模板仓库。
func NewBuiltinPromptStore() llm.TemplateStore {
	return llm.NewBuiltinStore(BuiltinPrompts)
}
