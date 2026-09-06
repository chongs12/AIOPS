<script setup>
// 推理回放播放器：按时间轴重放一次工单处理的完整推理过程（消息/工具/审计事件逐个点亮）。
import { ref, computed, onMounted, onUnmounted, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { getTicket, obsQuery } from '../api'
import EvidenceTimeline from '../components/EvidenceTimeline.vue'
import HypothesisFlow from '../components/HypothesisFlow.vue'
import ConfidenceGauge from '../components/ConfidenceGauge.vue'

const route = useRoute()
const router = useRouter()
const id = route.params.id

const feed = ref(null)
const tk = ref(null)
const cursor = ref(0)        // 当前播放到的事件下标（含）
const playing = ref(false)
const speed = ref(2)         // 播放速度倍数
const progress = ref(0)      // 0-100
let timer = null

const STEP_MS = 900 // 基础步长

const events = computed(() => feed.value?.events || [])
const visibleEvents = computed(() => events.value.slice(0, cursor.value + 1))
const current = computed(() => events.value[cursor.value] || null)

async function load() {
  tk.value = await getTicket(id)
  const res = await fetch(`/api/v1/tickets/${id}/replay`)
  feed.value = await res.json()
}

function play() {
  if (cursor.value >= events.value.length - 1) cursor.value = 0
  playing.value = true
  scheduleNext()
}

function pause() {
  playing.value = false
  clearTimeout(timer)
}

function scheduleNext() {
  if (!playing.value) return
  timer = setTimeout(() => {
    if (cursor.value < events.value.length - 1) {
      cursor.value++
      progress.value = Math.round(((cursor.value + 1) / events.value.length) * 100)
      scheduleNext()
    } else {
      playing.value = false
    }
  }, STEP_MS / speed.value)
}

function seek(p) {
  cursor.value = Math.min(events.value.length - 1, Math.round((p / 100) * events.value.length) - 1)
  progress.value = p
}

const currentObs = computed(() => {
  const e = current.value
  if (!e || e.type !== 'observation') return null
  return { source: e.actor.replace('tool:', ''), summary: e.detail, ref: e.ref_id }
})

const fmtTime = (ts) => new Date(ts).toLocaleTimeString('zh-CN')
const typeIcon = (t) => ({ message: '💬', observation: '🔬', audit: '🛡️', status: '⚙️' }[t] || '•')

const stageEl = ref(null)
function scrollStage() {
  nextTick(() => {
    if (stageEl.value) stageEl.value.scrollTop = stageEl.value.scrollHeight
  })
}

import { watch } from 'vue'
watch(cursor, scrollStage)
watch(feed, scrollStage)

onMounted(async () => {
  await load()
  play()
})
onUnmounted(() => clearTimeout(timer))
</script>

<template>
  <div class="replay">
    <!-- 顶部：标题 + 播放控制 -->
    <div class="rp-head card">
      <div>
        <div style="font-weight: 700">🎬 推理回放 · {{ tk?.title }}</div>
        <div class="muted mono">{{ id }}</div>
      </div>
      <div class="rp-controls">
        <el-button size="small" type="primary" @click="playing ? pause() : play()">
          {{ playing ? '⏸ 暂停' : '▶ 播放' }}
        </el-button>
        <el-select v-model="speed" size="small" style="width: 90px">
          <el-option :value="1" label="1x" /><el-option :value="2" label="2x" />
          <el-option :value="4" label="4x" /><el-option :value="8" label="8x" />
        </el-select>
        <el-slider v-model="progress" style="width: 220px" :format-tooltip="v => v + '%'" @input="seek" />
        <span class="muted mono">{{ cursor + 1 }}/{{ events.length }}</span>
        <a v-if="tk?.trace_url" :href="tk.trace_url" target="_blank">
          <el-button size="small" plain>🔍 Trace</el-button>
        </a>
      </div>
    </div>

    <div class="rp-body">
      <!-- 左：步骤时间轴 -->
      <div class="rp-timeline card">
        <div v-for="(e, i) in events" :key="i" class="rp-step" :class="{ done: i <= cursor, current: i === cursor }">
          <span class="rp-step-icon">{{ typeIcon(e.type) }}</span>
          <div>
            <div class="rp-step-title">{{ e.title }}</div>
            <div class="muted" style="font-size: 11px">{{ fmtTime(e.ts) }}</div>
          </div>
        </div>
      </div>

      <!-- 右：舞台 -->
      <div class="rp-stage card">
        <div ref="stageEl" class="rp-stage-scroll">
          <div v-for="(e, i) in visibleEvents" :key="i" class="rp-event" :class="e.type">
            <div class="rp-event-head">
              <span class="rp-event-badge">{{ typeIcon(e.type) }} {{ e.type }}</span>
              <span class="muted mono" style="font-size: 11px">{{ fmtTime(e.ts) }}</span>
            </div>
            <div v-if="e.type === 'message'" class="rp-event-body" :class="{ user: e.actor === 'user' }">
              {{ e.detail }}
            </div>
            <div v-else-if="e.type === 'observation'" class="rp-event-body tool">
              🔬 {{ e.actor.replace('tool:', '') }}
              <div class="muted" style="margin-top: 4px">{{ e.detail }}</div>
            </div>
            <div v-else class="rp-event-body sys">{{ e.title }}</div>
          </div>

          <!-- 结束态：报告摘要 -->
          <div v-if="cursor >= events.length - 1 && tk?.report" class="report-card">
            <div class="report-head">
              <div class="report-title">📋 最终报告</div>
              <ConfidenceGauge :confidence="tk.report.confidence || 0" :size="52" />
            </div>
            <div class="report-section"><h4>根因</h4>
              <div style="font-size: 13px">{{ tk.report.root_cause }}</div></div>
            <div class="report-section" v-if="tk.report.hypotheses?.length">
              <h4>假设验证</h4>
              <HypothesisFlow :hypotheses="tk.report.hypotheses" />
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.replay { display: flex; flex-direction: column; gap: 14px; }
.rp-head { display: flex; align-items: center; justify-content: space-between; }
.rp-controls { display: flex; align-items: center; gap: 10px; }
.rp-body { display: grid; grid-template-columns: 300px 1fr; gap: 14px; align-items: start; }
.rp-timeline { max-height: 66vh; overflow-y: auto; }
.rp-step {
  display: flex; gap: 8px; padding: 6px 8px; border-radius: 8px; margin-bottom: 2px;
  opacity: 0.35; transition: all 0.25s ease;
}
.rp-step.done { opacity: 1; }
.rp-step.current { opacity: 1; background: rgba(99, 102, 241, 0.08); box-shadow: inset 3px 0 0 var(--primary); }
.rp-step-icon { font-size: 13px; }
.rp-step-title { font-size: 12px; font-weight: 600; color: #334155; }
.rp-stage { min-height: 60vh; }
.rp-stage-scroll { max-height: 66vh; overflow-y: auto; padding: 4px 2px; }
.rp-event { margin-bottom: 10px; animation: fadein 0.3s ease; }
@keyframes fadein { from { opacity: 0; transform: translateY(6px); } to { opacity: 1; transform: none; } }
.rp-event-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 4px; }
.rp-event-badge { font-size: 11px; font-weight: 700; color: var(--primary-dark); background: rgba(99, 102, 241, 0.08); padding: 2px 9px; border-radius: 8px; }
.rp-event-body { font-size: 13px; line-height: 1.6; background: #f8fafc; border: 1px solid rgba(148, 163, 184, 0.16); border-radius: 10px; padding: 9px 12px; white-space: pre-wrap; word-break: break-word; }
.rp-event-body.user { background: linear-gradient(135deg, #dbeafe, #f0f9ff); }
.rp-event-body.tool { border-left: 3px solid var(--primary-light); }
.rp-event-body.sys { background: #fffbeb; border-color: rgba(245, 158, 11, 0.3); color: #92400e; }
</style>
