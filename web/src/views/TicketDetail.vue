<script setup>
import { ref, onMounted, onUnmounted, nextTick } from 'vue'
import { useRoute } from 'vue-router'
import {
  getTicket, listObservations, listAudit, replyTicket, runAction, streamTicket,
  statusType, statusLabel, typeLabel, agentLabel, obsQuery,
} from '../api'
import echarts from '../lib/echarts'

const route = useRoute()
const id = route.params.id
const tk = ref(null)
const observations = ref([])
const audit = ref([])
const input = ref('')
const busy = ref(false)
const live = ref(false)
let es = null
const trendEl = ref(null)
let trendChart = null

// 故障时间窗错误率趋势（真实 range 查询 + 窗口高亮）。
async function loadTrend() {
  if (!tk.value || !trendEl.value) return
  const win = (tk.value.entities?.window_minutes || 30) * 60 * 1000
  const end = Date.now() + 5 * 60 * 1000
  const start = new Date(tk.value.created_at).getTime() - win
  const step = Math.max(15, Math.floor((end - start) / 1000 / 120))
  try {
    const res = await obsQuery({
      template: 'http_5xx_rate',
      start: new Date(start).toISOString(), end: new Date(end).toISOString(), step_seconds: step,
    })
    if (res.status !== 'ok') return
    const rows = res.data || []
    if (!trendChart) trendChart = echarts.init(trendEl.value)
    const wStart = new Date(tk.value.created_at).getTime() - win
    const wEnd = new Date(tk.value.created_at).getTime()
    trendChart.setOption({
      grid: { left: 50, right: 16, top: 24, bottom: 26 },
      tooltip: { trigger: 'axis' },
      xAxis: { type: 'time', axisLabel: { fontSize: 10 } },
      yAxis: { type: 'value', axisLabel: { fontSize: 10 }, splitLine: { lineStyle: { color: '#eef2f7' } } },
      series: [{
        name: '5xx 速率', type: 'line', smooth: true, showSymbol: false,
        data: (rows[0]?.values || []).map(v => [v[0] * 1000, parseFloat(v[1])]),
        areaStyle: { opacity: 0.08 },
        markArea: {
          itemStyle: { color: 'rgba(239,68,68,0.07)' },
          data: [[{ xAxis: wStart, name: '故障时间窗' }, { xAxis: wEnd }]],
        },
      }],
    }, true)
  } catch { /* 趋势图为增强视图，失败不阻塞详情 */ }
}

async function load() {
  tk.value = await getTicket(id)
  observations.value = (await listObservations(id)).items || []
  try { audit.value = (await listAudit(id)).items || [] } catch { /* 内存模式无审计查询 */ }
  scrollBottom()
}

function renderMsg(m) {
  // SSE 增量追加（按 seq 去重）
  const msgs = tk.value.messages || []
  if (msgs.some(x => x.seq === m.seq)) return
  msgs.push(m)
  scrollBottom()
}

function scrollBottom() {
  nextTick(() => {
    const el = document.querySelector('.chat-body')
    if (el) el.scrollTop = el.scrollHeight
  })
}

function connect() {
  es = streamTicket(id, {
    onMessage: renderMsg,
    onStatus: async (s) => {
      live.value = ['investigating'].includes(s)
      await load()
    },
    onError: () => { live.value = false },
  })
}

async function send() {
  const v = input.value.trim()
  if (!v) return
  await replyTicket(id, v)
  input.value = ''
  await load()
}

// ---- 处置动作（人工确认门在前端 + 后端审计双保险）----
const confirmDlg = ref(false)
const pendingAction = ref(null)

function askAction(rec) {
  const target = (rec.target || '').replace(/^deployment\//, '')
  pendingAction.value = { rec, deployment: target, replicas: rec.action === 'scale' ? 1 : null }
  confirmDlg.value = true
}

async function execAction() {
  const p = pendingAction.value
  const tool = p.rec.action === 'rollout_undo' ? 'k8s.rollout_undo' : 'k8s.scale'
  const input_ = tool === 'k8s.scale'
    ? { deployment: p.deployment, replicas: Number(p.replicas) }
    : { deployment: p.deployment }
  busy.value = true
  try {
    const res = await runAction(id, tool, input_)
    ElMessage[res.status === 'ok' ? 'success' : 'warning'](
      res.status === 'ok' ? '处置已执行' : `未执行: ${res.degraded_reason || ''}`)
    confirmDlg.value = false
    await load()
  } finally {
    busy.value = false
  }
}

import { ElMessage } from 'element-plus'

onMounted(async () => { await load(); connect(); loadTrend() })
onUnmounted(() => es && es.close())
</script>

<template>
  <div v-if="tk">
    <!-- 顶部信息 -->
    <div class="card" style="display: flex; align-items: center; gap: 12px; padding: 14px 20px">
      <div>
        <div style="font-weight: 700; font-size: 15px">{{ tk.title }}</div>
        <div class="muted">
          <span class="mono">{{ tk.id }}</span> · {{ typeLabel(tk.type) }}
          <template v-if="tk.entities?.service"> · 服务 <b>{{ tk.entities.service }}</b></template>
          <template v-if="tk.entities?.window_minutes"> · 时间窗 {{ tk.entities.window_minutes }} 分钟</template>
        </div>
      </div>
      <div style="margin-left: auto; display: flex; align-items: center; gap: 10px">
        <el-tag v-if="live" type="warning" effect="dark" size="small">● 实时处理中</el-tag>
        <el-tag :type="statusType(tk.status)">{{ statusLabel(tk.status) }}</el-tag>
        <a v-if="tk.trace_url" class="link" :href="tk.trace_url" target="_blank">
          <el-button size="small" plain>🔍 推理 Trace</el-button>
        </a>
      </div>
    </div>

    <!-- 聊天式推理流水 -->
    <div class="chat-panel">
      <div class="chat-panel-header">
        <div class="chat-header-avatar">AI</div>
        <div class="chat-title">多 Agent 处理流水</div>
        <span class="header-badge">intent → metrics → log → rootcause → reply</span>
        <span v-if="tk.status === 'resolved'" class="header-badge ok">已解决</span>
        <span v-else-if="tk.status === 'escalated_human'" class="header-badge bad">已转人工</span>
        <span v-else-if="tk.status === 'awaiting_info'" class="header-badge warn">等待补充信息</span>
      </div>

      <div class="chat-body">
        <!-- 事故报告卡 -->
        <div v-if="tk.report" class="report-card">
          <div class="report-head">
            <div class="report-title">📋 事故报告</div>
            <span class="conf-pill" :class="{ low: (tk.report.confidence || 0) < 0.6 }">
              置信度 {{ ((tk.report.confidence || 0) * 100).toFixed(0) }}%
            </span>
          </div>
          <div class="report-section">
            <h4>根因</h4>
            <div style="font-size: 13px">{{ tk.report.root_cause }}</div>
          </div>
          <div class="report-section" v-if="tk.report.summary">
            <h4>摘要</h4>
            <div style="font-size: 13px; color: var(--text-secondary)">{{ tk.report.summary }}</div>
          </div>
          <div class="report-section" v-if="tk.report.timeline?.length">
            <h4>时间线</h4>
            <ul>
              <li v-for="t in tk.report.timeline" :key="t.event">
                {{ t.event }} <span v-if="t.source" class="obs-ref">{{ t.source }}</span>
              </li>
            </ul>
          </div>
          <div class="report-section" v-if="tk.report.recommendations?.length">
            <h4>处置建议</h4>
            <div v-for="(rec, i) in tk.report.recommendations" :key="i"
                 style="display:flex; align-items:center; gap:8px; font-size:13px; padding:3px 0">
              <el-tag size="small" :type="rec.risk === 'low' ? 'success' : 'danger'">
                {{ rec.risk === 'low' ? '低危·可自动' : '高危·需人工' }}
              </el-tag>
              <span>{{ rec.description }} → <b>{{ rec.target || rec.action }}</b></span>
              <el-button size="small" type="primary" plain
                         v-if="['scale', 'rollout_undo', 'k8s.scale', 'k8s.rollout_undo'].includes(rec.action)"
                         @click="askAction(rec)">
                ⚡ 一键执行
              </el-button>
            </div>
          </div>
          <div class="report-section" v-if="tk.report.missing_evidence?.length">
            <h4>缺失证据</h4>
            <ul><li v-for="m in tk.report.missing_evidence" :key="m" class="muted">{{ m }}</li></ul>
          </div>
        </div>

        <!-- 消息气泡 -->
        <div v-for="m in tk.messages || []" :key="m.seq"
             class="message-item" :class="m.role">
          <div class="msg-avatar" :class="m.role === 'user' ? 'user' : (m.role === 'system' ? 'sys' : 'agent')">
            {{ m.role === 'user' ? '👤' : (m.role === 'system' ? '!' : 'AI') }}
          </div>
          <div class="msg-content">
            <div class="msg-meta">
              <span v-if="m.role === 'agent'" class="agent-chip">{{ agentLabel(m.agent) }}</span>
              {{ m.role === 'user' ? '提交人' : (m.role === 'system' ? '系统' : 'Agent') }}
              · {{ new Date(m.created_at).toLocaleTimeString('zh-CN') }}
            </div>
            <div class="msg-bubble">{{ m.content }}</div>
          </div>
        </div>
      </div>

      <div class="input-row" v-if="tk.status === 'awaiting_info' || true">
        <el-input v-model="input" type="textarea" :rows="2"
                  :placeholder="tk.status === 'awaiting_info' ? '补充工单信息，系统将从断点续跑…' : '补充信息…'" />
        <el-button type="primary" :disabled="!input.trim()" @click="send">发送<br>断点续跑</el-button>
      </div>
    </div>

    <!-- 故障窗错误率趋势 -->
    <div class="card" style="margin-top: 18px">
      <div class="card-title">📉 5xx 错误率趋势（红色区域 = 工单故障时间窗）
        <span class="muted" style="margin-left: 8px">真实 Prometheus range 查询</span>
      </div>
      <div ref="trendEl" style="height: 220px"></div>
    </div>

    <!-- 观察集 + 审计 -->
    <div class="card">
      <div class="card-title">🔬 观察集（真实工具查询）</div>
      <el-collapse>
        <el-collapse-item v-for="o in observations" :key="o.id" :name="o.id">
          <template #title>
            <el-tag size="small" :type="o.status === 'ok' ? 'success' : 'danger'" style="margin-right: 8px">
              {{ o.status }}
            </el-tag>
            <span class="mono" style="margin-right: 10px">{{ o.source }}</span>
            <span class="muted" style="overflow: hidden; text-overflow: ellipsis">{{ o.summary }}</span>
          </template>
          <div class="mono muted">{{ o.query || '(无查询表达式)' }}</div>
        </el-collapse-item>
      </el-collapse>
    </div>

    <div class="card" v-if="audit.length">
      <div class="card-title">🛡️ 处置审计</div>
      <el-table :data="audit" size="small">
        <el-table-column prop="tool" label="工具" width="160" />
        <el-table-column prop="status" label="结果" width="90" />
        <el-table-column prop="duration_ms" label="耗时(ms)" width="100" />
        <el-table-column prop="input" label="输入" min-width="200">
          <template #default="{ row }"><span class="mono">{{ row.input }}</span></template>
        </el-table-column>
        <el-table-column prop="error" label="备注" min-width="160" />
      </el-table>
    </div>
  </div>

  <el-dialog v-model="confirmDlg" title="人工确认处置动作" width="460">
    <p style="font-size: 13px; margin-bottom: 10px">
      {{ pendingAction?.rec.description }}
    </p>
    <el-form label-width="90px">
      <el-form-item label="目标"><span class="mono">{{ pendingAction?.deployment }}</span></el-form-item>
      <el-form-item v-if="pendingAction?.rec.action === 'scale'" label="目标副本数">
        <el-input-number v-model="pendingAction.replicas" :min="0" :max="10" />
      </el-form-item>
      <el-alert type="warning" :closable="false"
                title="变更操作将写入审计日志，执行后自动复查服务状态" />
    </el-form>
    <template #footer>
      <el-button @click="confirmDlg = false">取消</el-button>
      <el-button type="primary" :loading="busy" @click="execAction">确认执行</el-button>
    </template>
  </el-dialog>
</template>
