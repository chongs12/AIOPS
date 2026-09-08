<script setup>
// AI Token 消费看板：从 /observability/ai_usage 读本进程累计值（本地 dev 免抓取即可见）。
import { ref, onMounted, onUnmounted } from 'vue'
import echarts from '../lib/echarts'
import { aiUsage } from '../api'

const usage = ref(null)
const distEl = ref(null)
const trendEl = ref(null)
let timer = null
let charts = []

const fmt = (n) => (n == null ? '—' : Math.round(n).toLocaleString('zh-CN'))

async function load() {
  try {
    usage.value = await aiUsage()
    renderDist()
  } catch { /* 单次刷新失败保留上一帧 */ }
}

function renderDist() {
  if (!distEl.value || !usage.value?.by_entry?.length) return
  let chart = charts.find(c => c.el === distEl.value)?.inst
  if (!chart) {
    chart = echarts.init(distEl.value)
    charts.push({ el: distEl.value, inst: chart })
  }
  const rows = [...usage.value.by_entry].sort((a, b) =>
    (b.prompt_tokens + b.completion_tokens) - (a.prompt_tokens + a.completion_tokens)).slice(0, 8)
  chart.setOption({
    grid: { left: 110, right: 20, top: 8, bottom: 8 },
    tooltip: { trigger: 'axis' },
    xAxis: { type: 'value', axisLabel: { fontSize: 10, color: '#8b93a3' } },
    yAxis: {
      type: 'category',
      data: rows.map(r => `${r.agent || 'unknown'}/${r.model || '?'}`),
      axisLabel: { fontSize: 10, color: '#4b5563' },
    },
    series: [
      { name: 'prompt', type: 'bar', stack: 't', barWidth: 14, itemStyle: { color: '#3b6ef6' },
        data: rows.map(r => r.prompt_tokens) },
      { name: 'completion', type: 'bar', stack: 't', barWidth: 14, itemStyle: { color: '#16875b' },
        data: rows.map(r => r.completion_tokens) },
    ],
  }, true)
}

function renderTrend() {
  // 趋势数据依赖 Prometheus 抓取（本地 dev 无抓取时隐藏趋势卡，聚合端点已有总量）
  // 预留：接入 Prometheus 后走 obsQuery({template:'ai_tokens_rate'})
}

onMounted(() => {
  load()
  timer = setInterval(load, 15000)
})
onUnmounted(() => {
  clearInterval(timer)
  charts.forEach(c => c.inst.dispose())
})
</script>

<template>
  <div class="card" v-if="usage">
    <div class="card-title">AI Token 消费
      <span class="muted" style="margin-left: 8px">本进程累计 · 每 15s 刷新</span>
    </div>
    <div class="stat-grid" style="margin-bottom: 12px">
      <div class="stat-card">
        <div class="stat-value">{{ fmt(usage.total_prompt_tokens) }}</div>
        <div class="stat-label">Prompt Tokens</div>
      </div>
      <div class="stat-card">
        <div class="stat-value" style="color: var(--ok)">{{ fmt(usage.total_completion_tokens) }}</div>
        <div class="stat-label">Completion Tokens</div>
      </div>
      <div class="stat-card">
        <div class="stat-value">{{ fmt(usage.total_calls) }}</div>
        <div class="stat-label">LLM 调用次数</div>
      </div>
      <div class="stat-card">
        <div class="stat-value">{{ (usage.avg_call_seconds || 0).toFixed(1) }}s</div>
        <div class="stat-label">平均调用耗时</div>
      </div>
    </div>
    <div class="card-title" style="font-size: 12px">按 Agent/模型 分布</div>
    <div ref="distEl" style="height: 200px"></div>
    <div class="muted" style="margin-top: 8px; font-size: 11px">
      数据源: 进程内 Prometheus registry（ai_tokens_total{provider,model,agent,type}）· 集群内部署由 Prometheus 抓取长期聚合
    </div>
  </div>
</template>
