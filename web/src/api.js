import axios from 'axios'

const http = axios.create({ timeout: 30000 })

// ---- 工单 ----
export const createTicket = (data) => http.post('/api/v1/tickets', data).then(r => r.data)
export const listTickets = (limit = 50) => http.get('/api/v1/tickets', { params: { limit } }).then(r => r.data)
export const getTicket = (id) => http.get(`/api/v1/tickets/${id}`).then(r => r.data)
export const listMessages = (id) => http.get(`/api/v1/tickets/${id}/messages`).then(r => r.data)
export const listObservations = (id) => http.get(`/api/v1/tickets/${id}/observations`).then(r => r.data)
export const listAudit = (id) => http.get(`/api/v1/tickets/${id}/audit`).then(r => r.data)
export const replyTicket = (id, content) => http.post(`/api/v1/tickets/${id}/reply`, { content }).then(r => r.data)
export const runAction = (id, tool, input) => http.post(`/api/v1/tickets/${id}/actions`, { tool, input }).then(r => r.data)

// ---- 管理面 / 案例库 ----
export const listPrompts = () => http.get('/api/v1/admin/prompts').then(r => r.data)
export const updatePrompt = (scene, prompt, config) =>
  http.put(`/api/v1/admin/prompts/${scene}`, config ? { prompt, config } : { prompt }).then(r => r.data)
export const resetPrompt = (scene) => http.delete(`/api/v1/admin/prompts/${scene}`).then(r => r.data)
export const searchCases = (q) => http.get('/api/v1/cases/search', { params: { q } }).then(r => r.data)

// ---- SSE ----
export function streamTicket(id, { onMessage, onStatus, onError }) {
  const es = new EventSource(`/api/v1/tickets/${id}/stream`)
  es.addEventListener('message', e => onMessage && onMessage(JSON.parse(e.data)))
  es.addEventListener('status', e => onStatus && onStatus(JSON.parse(e.data).status))
  es.onerror = () => { es.close(); onError && onError() }
  return es
}

// ---- 展示辅助 ----
export const statusType = (s) => ({
  submitted: 'info', investigating: 'warning', awaiting_info: 'primary',
  resolved: 'success', escalated_human: 'danger', failed: 'danger',
}[s] || 'info')
export const statusLabel = (s) => ({
  submitted: '已提交', investigating: '处理中', awaiting_info: '等待补充',
  resolved: '已解决', escalated_human: '转人工', failed: '失败',
}[s] || s)
export const typeLabel = (t) => ({
  '5xx_surge': '接口 5xx 突增', pod_oom: 'Pod OOM', slow_query: '慢查询', other: '其它',
}[t] || t)
export const agentLabel = (a) => ({
  intent: '🧭 意图识别', metrics: '📊 指标检索', log: '📜 日志解析',
  rootcause: '🔍 根因推理', reply: '📝 报告生成', action: '⚡ 处置执行',
}[a] || a || '系统')

// ---- 可观测 ----
export const listObsTemplates = () => http.get('/api/v1/observability/templates').then(r => r.data)
export const obsOverview = () => http.get('/api/v1/observability/overview').then(r => r.data)
export const obsQuery = (payload) => http.post('/api/v1/observability/query', payload).then(r => r.data)
export const KNOWN_SERVICES = ['gateway', 'user', 'membership', 'question', 'interview', 'realtime',
  'growth', 'plan', 'companion', 'community', 'learning-archive', 'ai-gateway', 'rag', 'coderunner', 'admin']

// ---- K8s 平台 / 链路 ----
export const listDeployments = () => http.get('/api/v1/k8s/deployments').then(r => r.data)
export const listK8sEvents = (pod_prefix, since_minutes = 60) =>
  http.get('/api/v1/k8s/events', { params: { pod_prefix, since_minutes } }).then(r => r.data)
export const queryK8sLogs = (pod_prefix, since_minutes = 30, tail_lines = 200) =>
  http.get('/api/v1/k8s/logs', { params: { pod_prefix, since_minutes, tail_lines } }).then(r => r.data)
export const runK8sAction = (tool, input) => http.post('/api/v1/k8s/actions', { tool, input }).then(r => r.data)
export const jaegerServices = () => http.get('/api/v1/jaeger/services').then(r => r.data)
export const jaegerTraces = (service) => http.get('/api/v1/jaeger/traces', { params: { service } }).then(r => r.data)

export const rateTicket = (id, score, comment) =>
  http.patch(`/api/v1/tickets/${id}/rating`, { score, comment }).then(r => r.data)
