<script setup>
import { ref, onMounted, onUnmounted, nextTick, watch } from 'vue'
import { renderMarkdown } from '../lib/markdown'
import ConfidenceGauge from '../components/ConfidenceGauge.vue'
import { useRouter } from 'vue-router'
import {
  createTicket, listTickets, getTicket, listObservations, replyTicket, streamTicket,
  statusType, statusLabel, typeLabel, agentLabel,
} from '../api'
import { ElMessage } from 'element-plus'

const router = useRouter()
const sessions = ref([])
const cur = ref(null)          // 当前工单 id
const tk = ref(null)           // 工单详情
const observations = ref([])
const messages = ref([])
const composer = ref('')
const live = ref(false)
const processOpen = ref(false)
watch(live, (v) => { processOpen.value = v })
const listEl = ref(null)
let es = null

const quickChips = [
  'gateway 接口 5xx 突增，请排查根因',
  'pod makejob-oom-demo 内存溢出 OOMKilled',
  '数据库出现慢查询，影响下单接口',
]

const suggestQuestions = [
  { icon: '', text: 'gateway 接口 5xx 突增，请排查根因' },
  { icon: '', text: 'pod makejob-oom-demo 内存溢出 OOMKilled' },
  { icon: '', text: '数据库出现慢查询，影响下单接口' },
]

async function loadSessions() {
  const d = await listTickets(50)
  sessions.value = d.items || []
}

// 新对话：脱离当前会话回到空态（发送即创建新工单会话）。
function newSession() {
  if (es) es.close()
  cur.value = ''
  tk.value = null
  messages.value = []
  observations.value = []
  live.value = false
}

async function selectSession(id) {
  cur.value = id
  live.value = false
  if (es) es.close()
  await reload()
  es = streamTicket(id, {
    onMessage: (m) => {
      if (!messages.value.some(x => x.seq === m.seq)) messages.value.push(m)
      scrollBottom()
    },
    onStatus: (s) => {
      // 轻量更新：只改状态字段，不整块 reload（保留逐条渲染效果）
      live.value = s === 'investigating'
      if (tk.value) tk.value.status = s
      if (s !== 'investigating') loadSessions()
      if (s === 'resolved') {
        // 终态时拉一次报告卡所需数据（不替换消息数组）
        getTicket(cur.value).then(t => { tk.value = { ...t, messages: messages.value } })
      }
    },
    onError: () => { live.value = false },
  })
}

async function reload() {
  if (!cur.value) return
  tk.value = await getTicket(cur.value)
  // 仅在初始加载（无已渲染消息）时整块替换，避免覆盖 SSE 逐条效果
  if (!messages.value.length) messages.value = tk.value.messages || []
  observations.value = (await listObservations(cur.value)).items || []
  scrollBottom()
}

// 自动滚底策略（对齐 ChatGPT/DeepSeek）：
// 仅当用户本来就贴在底部（<80px）或显式要求时才滚动，避免打断用户查看历史。
function scrollBottom(force = false) {
  nextTick(() => {
    const el = listEl.value
    if (!el) return
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80
    if (force || nearBottom) el.scrollTop = el.scrollHeight
  })
}

// 发送：无会话 / 会话已终态 → 新建工单会话；awaiting_info → 断点续跑
async function send() {
  const text = composer.value.trim()
  if (!text) return
  composer.value = ''
  const status = tk.value?.status
  if (cur.value && status === 'awaiting_info') {
    await replyTicket(cur.value, text)
    await reload()
    return
  }
  const d = await createTicket({ title: text.slice(0, 24), content: text, reporter: 'chat' })
  await loadSessions()
  await selectSession(d.id)
  scrollBottom(true)
}

function onKeydown(e) {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send() }
}

const reportOf = (t) => t?.report
const toolEvents = () => observations.value.map(o => ({
  name: o.source,
  ok: o.status === 'ok',
  detail: o.summary,
}))

onMounted(async () => { await loadSessions(); if (sessions.value.length) await selectSession(sessions.value[0].id) })
onUnmounted(() => es && es.close())
watch(cur, () => scrollBottom(true))
</script>

<template>
  <div class="aiops-page">
    <!-- 会话历史（工单即会话） -->
    <aside class="aiops-session-list">
      <div class="session-list-head">
        <span class="session-list-title">会话历史</span>
        <button type="button" class="session-new-btn" @click="newSession" title="开始新对话">＋ 新对话</button>
      </div>
      <div v-for="s in sessions" :key="s.id" class="aiops-session-item" :class="{ active: cur === s.id }">
        <button type="button" class="session-select-btn" @click="selectSession(s.id)">
          <span class="session-title">{{ s.title || '新会话' }}</span>
          <span class="session-sub">
            <span class="mini-dot" :class="s.status" />{{ statusLabel(s.status) }} · {{ typeLabel(s.type) }}
          </span>
        </button>
      </div>
      <div v-if="!sessions.length" class="session-empty">暂无会话，直接在右侧描述你的问题</div>
    </aside>

    <!-- 对话主区 -->
    <div class="aiops-chat-main">
      <div class="chat-toolbar">
        <div class="session-indicator">
          <span class="session-indicator-label">{{ tk ? tk.title : '新对话' }}</span>
          <span class="muted mono" style="font-size: 11px">{{ cur || '描述问题后回车创建工单会话' }}</span>
        </div>
        <div class="chat-toolbar-right">
          <span v-if="live" class="environment-chip running">● 多 Agent 处理中</span>
          <span v-else-if="tk" class="environment-chip" :class="{ bad: tk.status !== 'resolved' }">
            {{ statusLabel(tk.status) }}
          </span>
          <el-button v-if="cur" size="small" class="aiops-toolbar-btn" @click="router.push(`/tickets/${cur}/replay`)">
            <el-icon><VideoPlay /></el-icon><span>回放</span>
          </el-button>
          <el-button size="small" class="aiops-toolbar-btn" @click="router.push('/')">
            <el-icon><Grid /></el-icon><span>工作台</span>
          </el-button>
        </div>
      </div>

      <div class="message-stage">
        <div ref="listEl" class="aiops-message-list">
          <div v-if="!cur" class="message-empty">
            <div class="empty-title">可以直接问我</div>
            <div class="empty-copy">
              描述一个故障现象，多 Agent 会查询真实集群的 Prometheus / 日志 / 事件，
              给出带证据引用与置信度的根因报告。每段对话都会沉淀为可检索的处置案例。
              <div class="muted" style="margin-top: 8px; font-size: 11px">接入 Alertmanager 后，告警会自动创建工单并进入本对话流。</div>
            </div>
            <div class="aiops-quick-questions" style="margin-top: 14px; justify-content: center">
              <button v-for="q in suggestQuestions" :key="q.text" type="button" class="quick-chip"
                      @click="composer = q.text; send()">
                <span style="margin-right: 4px">{{ q.icon }}</span>{{ q.text }}
              </button>
            </div>
          </div>

          <template v-else>
            <div v-for="m in messages" :key="m.seq" class="message-item" :class="m.role">
              <div class="message-meta">
                <span class="message-role">
                  <span v-if="m.role === 'agent'" class="agent-chip">{{ agentLabel(m.agent) }}</span>
                  {{ m.role === 'user' ? '你' : (m.role === 'system' ? '系统' : '智能助手') }}
                </span>
                <span class="message-time">{{ new Date(m.created_at).toLocaleTimeString('zh-CN') }}</span>
              </div>
              <div class="message-bubble">
                <div v-if="m.role === 'user'" class="message-content user-content">{{ m.content }}</div>
                <div v-else class="message-content" v-html="renderMarkdown(m.content)" />
              </div>
            </div>

            <!-- 思考过程卡：真实工具查询事件 -->
            <div v-if="observations.length" class="message-item assistant">
              <div class="analysis-process-card" :class="{ active: live }" style="width: 100%">
                <div class="analysis-process-head">
                  <div class="analysis-process-headline">
                    <div class="analysis-process-title">思考过程</div>
                    <div class="analysis-process-inline-summary">
                      {{ observations.length }} 项真实工具查询（Prometheus / K8s 日志与事件）
                    </div>
                  </div>
                  <div class="analysis-process-actions">
                    <span class="analysis-process-status" :class="live ? 'running' : 'completed'">
                      {{ live ? '运行中' : '已完成' }}
                    </span>
                    <button type="button" class="analysis-process-toggle" @click="processOpen = !processOpen">
                      {{ processOpen ? '收起' : '展开' }}
                    </button>
                  </div>
                </div>
                <transition name="expand">
                <div v-show="processOpen" class="tool-event-list">
                  <div v-for="(o, i) in observations" :key="o.id" class="tool-event-item"
                       :class="{ fresh: live && i === observations.length - 1 }"
                       :style="{ animationDelay: (i * 0.06) + 's' }">
                    <span class="tool-event-dot" :class="o.status === 'ok' ? 'ok' : 'bad'" />
                    <span class="tool-event-name">{{ o.source }}</span>
                    <span class="tool-event-detail">{{ o.summary.slice(0, 52) }}</span>
                  </div>
                  <div v-if="live" class="tool-event-item thinking">
                    <span class="tool-event-dot pulse" />
                    <span class="tool-event-name">正在执行</span>
                    <span class="tool-event-detail"><span class="typing-dots"><span /><span /><span /></span></span>
                  </div>
                </div>
                </transition>
              </div>
            </div>

            <!-- 最终报告（富卡片内联；仅在有实质根因内容时渲染，处理中不出现空卡） -->
            <div v-if="tk?.status === 'resolved' && reportOf(tk)?.root_cause" class="message-item assistant">
              <div style="width: 100%">
                <div class="report-card">
                  <div class="report-head">
                    <div class="report-title">事故报告</div>
                    <ConfidenceGauge :confidence="tk.report.confidence || 0" :size="52" />
                  </div>
                  <div class="report-section"><h4>根因</h4>
                    <div style="font-size: 13px">{{ tk.report.root_cause }}</div></div>
                  <div class="report-section" v-if="tk.report.recommendations?.length">
                    <h4>处置建议</h4>
                    <ul><li v-for="rec in tk.report.recommendations" :key="rec.description">
                      <el-tag size="small" :type="rec.risk === 'low' ? 'success' : 'danger'">
                        {{ rec.risk === 'low' ? '低危' : '高危' }}
                      </el-tag> {{ rec.description }}
                    </li></ul>
                  </div>
                  <div style="margin-top: 10px">
                    <el-button size="small" type="primary" plain @click="router.push(`/tickets/${tk.id}`)">
                      查看完整报告 / 执行处置 →
                    </el-button>
                  </div>
                </div>
              </div>
            </div>

            <div v-if="live" class="message-item assistant">
              <div class="message-bubble" style="width: 100%">
                <div class="typing-dots"><span /><span /><span /></div>
                <span class="muted" style="font-size: 12px">多 Agent 正在查询真实集群数据…</span>
              </div>
            </div>
          </template>
        </div>
      </div>

      <div class="aiops-composer">
        <el-input v-model="composer" type="textarea" :rows="2" resize="none"
                  placeholder="描述故障现象，Enter 发送，Shift + Enter 换行" @keydown="onKeydown" />
        <div class="composer-actions">
          <div class="composer-meta">
            <span class="composer-tip">Enter 发送，Shift + Enter 换行</span>
            <span v-if="live" class="composer-tip">正在流式返回结果</span>
            <span v-if="cur && tk && tk.status !== 'awaiting_info' && tk.status !== 'investigating'"
                  class="composer-tip">当前会话已结束，发送将创建新工单</span>
          </div>
          <div class="composer-action-group">
            <el-button type="primary" class="aiops-send-btn" :disabled="!composer.trim() || live" @click="send">
              <el-icon><Promotion /></el-icon><span>发送</span>
            </el-button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.aiops-page { display: flex; gap: 14px; height: calc(100vh - var(--header-h) - 40px); overflow: hidden; }
.aiops-session-list {
  width: 240px; flex-shrink: 0; min-height: 0; background: var(--card-bg);
  border-radius: var(--card-radius); box-shadow: var(--card-shadow);
  padding: 10px; overflow-y: auto;
}
.session-list-head { padding: 6px 8px 10px; display: flex; align-items: center; justify-content: space-between; }
.session-new-btn {
  font-size: 11px; font-weight: 600; color: var(--accent-strong);
  background: var(--accent-soft); border: 1px solid var(--accent-border);
  border-radius: 6px; padding: 4px 10px; cursor: pointer; transition: all 0.15s ease;
}
.session-new-btn:hover { background: rgba(59, 110, 246, 0.16); }
.session-list-title { font-size: 13px; font-weight: 700; color: #334155; }
.aiops-session-item { position: relative; border-radius: 12px; margin-bottom: 6px; transition: all .18s ease; }
.aiops-session-item:hover { background: #fff; transform: translateY(-1px); }
.aiops-session-item.active { background: #fff; box-shadow: 0 10px 20px rgba(15, 23, 42, 0.08); }
.session-select-btn { width: 100%; text-align: left; padding: 9px 10px; border: none; background: transparent; cursor: pointer; }
.session-title { display: block; font-size: 12px; font-weight: 600; color: #334155; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.session-sub { display: block; font-size: 11px; color: #94a3b8; margin-top: 2px; }
.mini-dot { display: inline-block; width: 6px; height: 6px; border-radius: 50%; margin-right: 5px; background: #cbd5e1; }
.mini-dot.resolved { background: var(--success); }
.mini-dot.escalated_human, .mini-dot.failed { background: var(--danger); }
.mini-dot.investigating, .mini-dot.awaiting_info { background: var(--warning); }

.aiops-chat-main {
  flex: 1; min-width: 0; min-height: 0; display: flex; flex-direction: column;
  background: var(--card-bg); border-radius: var(--card-radius); box-shadow: var(--card-shadow);
  overflow: hidden;
}
.chat-toolbar { display: flex; align-items: center; justify-content: space-between; padding: 8px 14px; border-bottom: 1px solid #edf2f7; background: rgba(255, 255, 255, 0.82); }
.chat-toolbar-right { display: flex; align-items: center; gap: 10px; }
.session-indicator-label { font-size: 13px; font-weight: 700; color: #334155; }
.environment-chip { display: inline-flex; align-items: center; height: 24px; padding: 0 9px; border-radius: 6px; background: #ecfdf5; border: 1px solid #bbf7d0; color: #047857; font-size: 12px; font-weight: 700; }
.environment-chip.running { background: #eff6ff; border-color: #bfdbfe; color: #1d4ed8; animation: pulse 1.6s infinite; }
.environment-chip.bad { background: #fff7ed; border-color: #fed7aa; color: #c2410c; }
@keyframes pulse { 50% { opacity: 0.65; } }

.message-stage { flex: 1; min-height: 0; display: flex; flex-direction: column; overflow: hidden; }
.aiops-message-list {
  flex: 1; min-height: 0;
  overflow-y: auto; overscroll-behavior: contain;
  padding: 12px 16px;
  display: flex; flex-direction: column; gap: 8px;
  scroll-behavior: smooth;
}
.message-empty { margin: auto; max-width: 400px; text-align: center; }
.empty-title { font-size: 17px; font-weight: 700; color: #0f172a; }
.empty-copy { margin-top: 8px; color: #64748b; line-height: 1.7; font-size: 12.5px; }
.aiops-quick-questions { display: flex; gap: 8px; flex-wrap: wrap; }
.quick-chip { border: none; border-radius: 999px; padding: 7px 12px; cursor: pointer; font-size: 12px; background: #e0f2fe; color: #075985; transition: all .18s ease; }
.quick-chip:hover { transform: translateY(-1px); box-shadow: 0 8px 20px rgba(15, 23, 42, 0.08); }

.message-item { display: flex; flex-direction: column; gap: 3px; max-width: 92%; flex-shrink: 0; }
.message-item.user { align-self: flex-end; }
.message-meta { display: flex; align-items: center; gap: 6px; color: #64748b; font-size: 11px; }
.message-item.user .message-meta { justify-content: flex-end; }
.message-bubble { padding: 8px 12px; border-radius: 14px; background: #fff; border: 1px solid #e2e8f0; box-shadow: 0 6px 14px rgba(15, 23, 42, 0.05); }
.message-item.user .message-bubble { background: linear-gradient(135deg, #dbeafe, #f0f9ff); }
.message-content { font-size: 13px; line-height: 1.6; color: #0f172a; white-space: pre-wrap; word-break: break-word; }
.agent-chip { display: inline-block; font-size: 10px; font-weight: 600; padding: 1px 7px; border-radius: 8px; margin-right: 6px; background: rgba(99, 102, 241, 0.1); color: var(--primary-dark); }

.analysis-process-card { padding: 9px 11px; border-radius: 12px; background: #f8fafc; border: 1px solid #e2e8f0; }
.analysis-process-card.active { border-color: #bfdbfe; }
.analysis-process-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.analysis-process-headline { display: flex; align-items: center; gap: 8px; min-width: 0; }
.analysis-process-title { font-size: 12px; font-weight: 600; color: #334155; }
.analysis-process-inline-summary { font-size: 11px; color: #64748b; }
.analysis-process-status { padding: 2px 7px; border-radius: 999px; background: #f1f5f9; color: #64748b; font-size: 11px; }
.analysis-process-status.running { background: #eff6ff; color: #1d4ed8; }
.analysis-process-toggle { border: none; background: transparent; color: #64748b; font-size: 11px; cursor: pointer; }
.tool-event-list { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 10px; }
.tool-event-item { display: flex; align-items: center; gap: 6px; padding: 4px 8px; border-radius: 999px; background: #fff; border: 1px solid #e2e8f0; font-size: 11px; color: #334155; }
.tool-event-name { font-weight: 600; color: #475569; }
.tool-event-detail { color: #64748b; }

.typing-dots { display: inline-flex; gap: 4px; margin-right: 8px; vertical-align: middle; }
.typing-dots span { width: 6px; height: 6px; border-radius: 50%; background: var(--primary-light); animation: blink 1.2s infinite both; }
.typing-dots span:nth-child(2) { animation-delay: 0.2s; }
.typing-dots span:nth-child(3) { animation-delay: 0.4s; }
@keyframes blink { 0%, 80%, 100% { opacity: 0.25; } 40% { opacity: 1; } }

.aiops-composer { padding: 10px 14px; border-top: 1px solid #e2e8f0; background: #fff; }
.composer-actions { display: flex; align-items: center; justify-content: space-between; margin-top: 5px; }
.composer-tip { font-size: 10px; color: #64748b; }
.aiops-send-btn { border-radius: 8px; }

.tool-event-item { animation: toolIn 0.35s cubic-bezier(0.2, 0.8, 0.2, 1) both; display: flex; align-items: center; gap: 7px; }
@keyframes toolIn { from { opacity: 0; transform: translateY(6px); } to { opacity: 1; transform: none; } }
.tool-event-item.fresh .tool-event-dot { box-shadow: 0 0 0 4px rgba(59, 110, 246, 0.15); }
.tool-event-dot { width: 7px; height: 7px; border-radius: 50%; flex-shrink: 0; }
.tool-event-dot.ok { background: var(--ok, #16875b); }
.tool-event-dot.bad { background: var(--bad, #d64550); }
.tool-event-dot.pulse { background: var(--accent, #3b6ef6); animation: pulse 1.2s infinite; }
.tool-event-item.thinking { color: var(--fg-3, #8b93a3); }
.expand-enter-active, .expand-leave-active { transition: all 0.25s ease; overflow: hidden; }
.expand-enter-from, .expand-leave-to { opacity: 0; max-height: 0; }
.expand-enter-to, .expand-leave-from { opacity: 1; max-height: 600px; }
</style>
