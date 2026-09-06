<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import echarts from '../lib/echarts'
import { obsOverview, obsQuery } from '../api'
import SkeletonCard from '../components/SkeletonCard.vue'

const overview = ref(null)
const errChart = ref(null)
const p99Chart = ref(null)
const loading = ref(false)
const tsRange = ref(60) // 分钟
let timer = null
let charts = []

async function load() {
  loading.value = true
  try {
    overview.value = await obsOverview()
    await loadTrends()
  } finally {
    loading.value = false
  }
}

async function loadTrends() {
  const end = Date.now()
  const start = end - tsRange.value * 60 * 1000
  const q = (template) => obsQuery({
    template, start: new Date(start).toISOString(), end: new Date(end).toISOString(),
    step_seconds: Math.max(15, Math.floor(tsRange.value * 60 / 120)),
  })
  const [errRes, p99Res] = await Promise.all([q('http_5xx_rate'), q('http_latency_p99')])

  renderLine(errChart.value, errRes, '5xx 速率 (req/s)', '#ef4444')
  renderLine(p99Chart.value, p99Res, 'P99 延迟 (s)', '#6366f1')
}

function renderLine(el, res, title, color) {
  if (!el || !res || res.status !== 'ok') return
  let chart = charts.find(c => c.el === el)?.inst
  if (!chart) {
    chart = echarts.init(el)
    charts.push({ el, inst: chart })
  }
  const rows = res.data || []
  const series = (rows || []).slice(0, 6).map((r, i) => ({
    name: seriesName(r.metric, i),
    type: 'line', showSymbol: false, smooth: true, lineWidth: 1.6,
    data: (r.values || []).map(v => [v[0] * 1000, parseFloat(v[1])]),
  }))
  chart.setOption({
    grid: { left: 50, right: 16, top: 30, bottom: 26 },
    tooltip: { trigger: 'axis' },
    legend: { show: series.length > 1, top: 0, textStyle: { fontSize: 11 } },
    xAxis: { type: 'time', axisLabel: { fontSize: 10 } },
    yAxis: { type: 'value', axisLabel: { fontSize: 10 }, splitLine: { lineStyle: { color: '#eef2f7' } } },
    color: [color, '#f59e0b', '#10b981', '#3b82f6', '#8b5cf6', '#ec4899'],
    series,
  }, true)
  chart._title = title
}

function seriesName(metric, i) {
  if (!metric || Object.keys(metric).length === 0) return '序列 ' + (i + 1)
  return Object.entries(metric).filter(([k]) => k !== '__name__')
    .map(([k, v]) => `${k}=${v}`).join(' ') || '合计'
}

const fmt = (v, unit) => v == null ? '—' : v.toFixed(v < 10 ? 4 : 1) + (unit || '')

onMounted(() => {
  load()
  timer = setInterval(load, 30000) // 30s 自动刷新
})
onUnmounted(() => {
  clearInterval(timer)
  charts.forEach(c => c.inst.dispose())
})
</script>

<template>
  <div>
    <template v-if="loading && !overview">
      <SkeletonCard :rows="2" chart />
      <SkeletonCard :rows="3" />
    </template>
    <div class="stat-grid">
      <div class="stat-card">
        <div class="stat-icon" style="background: linear-gradient(135deg, #6366f1, #4f46e5)">🖥️</div>
        <div>
          <div class="stat-value">{{ (overview?.services || []).length }}</div>
          <div class="stat-label">服务实例（Prometheus 目标）</div>
        </div>
      </div>
      <div class="stat-card">
        <div class="stat-icon" style="background: linear-gradient(135deg, #ef4444, #dc2626)">🔥</div>
        <div>
          <div class="stat-value">{{ fmt(overview?.err_5xx_rate, ' rps') }}</div>
          <div class="stat-label">5xx 速率（5m 窗口）</div>
        </div>
      </div>
      <div class="stat-card">
        <div class="stat-icon" style="background: linear-gradient(135deg, #10b981, #059669)">🌐</div>
        <div>
          <div class="stat-value">{{ fmt(overview?.total_rate, ' rps') }}</div>
          <div class="stat-label">总请求速率</div>
        </div>
      </div>
      <div class="stat-card">
        <div class="stat-icon" style="background: linear-gradient(135deg, #f59e0b, #d97706)">⚡</div>
        <div>
          <div class="stat-value">{{ fmt(overview?.p99_seconds, ' s') }}</div>
          <div class="stat-label">网关 P99 延迟</div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-title">📈 5xx 错误率趋势
        <el-select v-model="tsRange" size="small" style="width: 110px; margin-left: auto" @change="loadTrends">
          <el-option :value="15" label="近 15 分钟" />
          <el-option :value="60" label="近 1 小时" />
          <el-option :value="360" label="近 6 小时" />
        </el-select>
      </div>
      <div ref="errChart" style="height: 240px"></div>
    </div>

    <div class="card">
      <div class="card-title">📉 网关 P99 延迟趋势</div>
      <div ref="p99Chart" style="height: 240px"></div>
    </div>

    <div class="card">
      <div class="card-title">🟢 服务存活状态（真实 up 指标聚合）
        <span class="muted" style="margin-left: 8px">数据源: prometheus.localhost</span>
      </div>
      <div style="display: flex; flex-wrap: wrap; gap: 8px">
        <div v-for="s in overview?.services || []" :key="s.service"
             style="display: flex; align-items: center; gap: 6px; padding: 6px 12px; border-radius: 8px"
             :style="s.up ? 'background: #ecfdf5; border: 1px solid #bbf7d0' : 'background: #fef2f2; border: 1px solid #fecaca'">
          <span :style="{ width: '8px', height: '8px', borderRadius: '50%', background: s.up ? '#10b981' : '#ef4444' }" />
          <span style="font-size: 12px; font-weight: 600; color: s.up ? '#047857' : '#b91c1c'">
            {{ s.service.replace('makejob-', '') }}
          </span>
        </div>
      </div>
    </div>
  </div>
</template>
