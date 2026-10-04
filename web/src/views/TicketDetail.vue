<script setup>
import { ref, computed, onMounted, onUnmounted, nextTick } from 'vue'
import { useRoute } from 'vue-router'
import {
  getTicket, listObservations, listAudit, replyTicket, streamTicket,
  statusType, statusLabel, typeLabel, agentLabel, obsQuery, rateTicket,
  createRemediation, listRemediations, approveRemediation, rejectRemediation,
  remediationPhaseType, remediationPhaseLabel,
} from '../api'
import echarts from '../lib/echarts'
import ConfidenceGauge from '../components/ConfidenceGauge.vue'
import EvidenceTimeline from '../components/EvidenceTimeline.vue'
import HypothesisFlow from '../components/HypothesisFlow.vue'
import EvidenceDrawer from '../components/EvidenceDrawer.vue'

const route = useRoute()
const id = route.params.id
const tk = ref(null)
const observations = ref([])
const audit = ref([])
const input = ref('')
const busy = ref(false)
const live = ref(false)
let es = null
const rating = ref({ score: 5, note: '' })
const evidenceDrawer = ref(null)
const hypotheses = computed(() => tk.value?.report?.hypotheses || [])
function openEvidence(obsId) {
  evidenceDrawer.value?.open(obsId)
}
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
  try { remediations.value = (await listRemediations(id)).items || [] } catch { /* CR 能力未装配（本地无 K8s） */ }
  scrollBottom()
}

function renderMsg(m) {
  // SSE 增量追加（按 seq 去重）
  const msgs = tk.value.messages || []
  if (msgs.some(x => x.seq === m.seq)) return
  msgs.push(m)
  // 处置 CR 阶段变化消息到达时同步刷新审批区块
  if (m.agent === 'remediation') {
    listRemediations(id).then(r => { remediations.value = r.items || [] }).catch(() => {})
  }
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

async function submitRating() {
  await rateTicket(id, rating.value.score, rating.value.note)
  ElMessage.success('感谢反馈')
  await load()
}

async function send() {
  const v = input.value.trim()
  if (!v) return
  await replyTicket(id, v)
  input.value = ''
  await load()
}

// ---- 处置（Remediation CR 平台化链路）：发起 → 人工审批门 → operator 执行/回验 ----
const confirmDlg = ref(false)
const pendingAction = ref(null)
const remediations = ref([])

function askAction(rec) {
  const target = (rec.target || '').replace(/^deployment\//, '')
  pendingAction.value = { rec, deployment: target, replicas: rec.action === 'scale' ? 1 : null }
  confirmDlg.value = true
}

async function createCR() {
  const p = pendingAction.value
  const action = (p.rec.action === 'rollout_undo' || p.rec.action === 'k8s.rollout_undo')
    ? 'k8s.rollout_undo' : 'k8s.scale'
  busy.value = true
  try {
    const res = await createRemediation(id, {
      deployment: p.deployment, action,
      replicas: action === 'k8s.scale' ? Number(p.replicas) : undefined,
      reason: p.rec.description,
    })
    ElMessage.success(res.message || '处置 CR 已创建，等待审批')
    confirmDlg.value = false
    await load()
  } catch (e) {
    ElMessage.error(e.response?.data?.error || '创建失败')
  } finally {
    busy.value = false
  }
}

async function approve(name) {
  try {
    await ElMessageBox.confirm(`确认审批通过 ${name}？通过后 operator 将执行处置并自动回验恢复效果。`, '人工审批门', { type: 'warning' })
  } catch { return }
  try {
    const res = await approveRemediation(name)
    ElMessage.success(res.message || '已审批通过')
    await load()
  } catch (e) {
    ElMessage.error(e.response?.data?.error || '审批失败')
  }
}

async function reject(name) {
  let note = ''
  try {
    const r = await ElMessageBox.prompt('拒绝原因（写入处置 CR 审计，可追溯）', '拒绝处置', { inputValue: '副本数不合理' })
    note = r.value || ''
  } catch { return }
  try {
    const res = await rejectRemediation(name, note)
    ElMessage.info(res.message || '已拒绝')
    await load()
  } catch (e) {
    ElMessage.error(e.response?.data?.error || '操作失败')
  }
}

import { ElMessage, ElMessageBox } from 'element-plus'

onMounted(async () => { await load(); connect(); loadTrend() })
onUnmounted(() => es && es.close())
</script>

<template>
  <div v-if="tk">
    <!-- 顶部信息 -->
    <div class="card" style="display: flex; align-items: center; gap: 12px; padding: 14px 20px">
      <div>
        <div style="font-weight: 700; font-size: 15px">
          <el-tag v-if="tk.reporter === 'alertmanager'" size="small" type="danger" effect="dark" style="margin-right: 6px">告警</el-tag>{{ tk.title }}
        </div>
        <div class="muted">
          <span class="mono">{{ tk.id }}</span> · {{ typeLabel(tk.type) }}
          <template v-if="tk.entities?.service"> · 服务 <b>{{ tk.entities.service }}</b></template>
          <template v-if="tk.entities?.window_minutes"> · 时间窗 {{ tk.entities.window_minutes }} 分钟</template>
        </div>
      </div>
      <div style="margin-left: auto; display: flex; align-items: center; gap: 10px">
        <el-tag v-if="live" type="warning" effect="dark" size="small">● 实时处理中</el-tag>
        <el-tag :type="statusType(tk.status)">{{ statusLabel(tk.status) }}</el-tag>
        <router-link :to="`/tickets/${id}/replay`">
          <el-button size="small" type="primary" plain>回放</el-button>
        </router-link>
        <a v-if="tk.trace_url" class="link" :href="tk.trace_url" target="_blank">
          <el-button size="small" plain>Trace</el-button>
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
            <div class="report-title">事故报告</div>
            <ConfidenceGauge :confidence="tk.report.confidence || 0" :size="56" />
          </div>
          <div class="report-section">
            <h4>根因</h4>
            <div style="font-size: 13px">{{ tk.report.root_cause }}</div>
          </div>
          <div class="report-section" v-if="tk.report.summary">
            <h4>摘要</h4>
            <div style="font-size: 13px; color: var(--text-secondary)">{{ tk.report.summary }}</div>
          </div>
          <div class="report-section" v-if="hypotheses.length">
            <h4>假设验证流</h4>
            <HypothesisFlow :hypotheses="hypotheses" @open-evidence="openEvidence" />
          </div>
          <div class="report-section" v-if="tk.report.timeline?.length">
            <h4>时间线</h4>
            <EvidenceTimeline :items="tk.report.timeline" @open-evidence="openEvidence" />
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
                发起处置
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

    <!-- 处置审批（Remediation CR 平台化链路） -->
    <div class="card" v-if="remediations.length" style="margin-top: 18px">
      <div class="card-title">处置审批
        <span class="muted" style="margin-left: 8px">Remediation CR · 审批门 → operator 执行 → 自动回验</span>
      </div>
      <el-table :data="remediations" size="small">
        <el-table-column prop="name" label="CR" min-width="180">
          <template #default="{ row }"><span class="mono">{{ row.name }}</span></template>
        </el-table-column>
        <el-table-column prop="action" label="动作" width="150" />
        <el-table-column prop="deployment" label="目标" min-width="140" />
        <el-table-column label="参数" width="90">
          <template #default="{ row }">{{ row.replicas != null ? row.replicas + ' 副本' : '—' }}</template>
        </el-table-column>
        <el-table-column label="阶段" width="100">
          <template #default="{ row }">
            <el-tag size="small" :type="remediationPhaseType(row.phase)" effect="dark">
              {{ remediationPhaseLabel(row.phase) }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="message" label="进展" min-width="200" show-overflow-tooltip />
        <el-table-column prop="approver" label="审批人" width="90" />
        <el-table-column label="操作" width="140">
          <template #default="{ row }">
            <template v-if="row.phase === 'PendingApproval'">
              <el-button size="small" type="success" @click="approve(row.name)">审批</el-button>
              <el-button size="small" type="danger" plain @click="reject(row.name)">拒绝</el-button>
            </template>
            <span v-else class="muted">—</span>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 满意度评价（resolved 后） -->
    <div class="card" v-if="tk.status === 'resolved'" style="margin-top: 18px">
      <div class="card-title">满意度
        <span v-if="tk.rating" class="muted" style="margin-left: 8px">
          已评价：{{ tk.rating }} 分{{ tk.rating_note ? ' · ' + tk.rating_note : '' }}
        </span>
      </div>
      <div v-if="!tk.rating" style="display: flex; align-items: center; gap: 12px">
        <el-rate v-model="rating.score" />
        <el-input v-model="rating.note" placeholder="备注（可空）" style="width: 300px" />
        <el-button type="primary" size="small" @click="submitRating">提交评价</el-button>
      </div>
    </div>

    <!-- 故障窗错误率趋势 -->
    <div class="card" style="margin-top: 18px">
      <div class="card-title">5xx 错误率 · 工单故障时间窗
        <span class="muted" style="margin-left: 8px">真实 Prometheus range 查询</span>
      </div>
      <div ref="trendEl" style="height: 220px"></div>
    </div>

    <!-- 观察集 + 审计 -->
    <div class="card">
      <div class="card-title">观察集</div>
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
      <div class="card-title">处置审计</div>
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

  <EvidenceDrawer ref="evidenceDrawer" :ticket-id="id" />

  <el-dialog v-model="confirmDlg" title="发起处置（Remediation CR）" width="460">
    <p style="font-size: 13px; margin-bottom: 10px">
      {{ pendingAction?.rec.description }}
    </p>
    <el-form label-width="90px">
      <el-form-item label="目标"><span class="mono">{{ pendingAction?.deployment }}</span></el-form-item>
      <el-form-item v-if="pendingAction?.rec.action !== 'rollout_undo' && pendingAction?.rec.action !== 'k8s.rollout_undo'" label="目标副本数">
        <el-input-number v-model="pendingAction.replicas" :min="0" :max="10" />
      </el-form-item>
      <el-alert type="warning" :closable="false"
                title="创建后需人工审批；审批通过由 operator 自动执行并回验，全程 kubectl get remediations 可观察" />
    </el-form>
    <template #footer>
      <el-button @click="confirmDlg = false">取消</el-button>
      <el-button type="primary" :loading="busy" @click="createCR">创建待审批 CR</el-button>
    </template>
  </el-dialog>
</template>
