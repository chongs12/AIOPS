<script setup>
// NOC 指挥大屏：独立深色路由页（不改动浅色页面），F11 即投屏态。
import { ref, onMounted, onUnmounted, nextTick } from 'vue'
import { useRouter } from 'vue-router'
const router = useRouter()
import echarts from '../lib/echarts'
import { obsOverview, obsQuery, listK8sEvents } from '../api'

const overview = ref(null)
const warnings = ref([])
const errChart = ref(null)
const p99Chart = ref(null)
const clock = ref('')
let timer = null
let clockTimer = null
let charts = []

function initChart(el) {
  const c = echarts.init(el, null, { renderer: 'canvas' })
  charts.push({ el, inst: c })
  return c
}

function lineOption(data, color) {
  const rows = data || []
  return {
    grid: { left: 46, right: 14, top: 18, bottom: 22 },
    tooltip: { trigger: 'axis', backgroundColor: '#132033', borderColor: '#25405e', textStyle: { color: '#cfe3ff' } },
    xAxis: { type: 'time', axisLabel: { color: '#5f7fa6', fontSize: 10 }, axisLine: { lineStyle: { color: '#1d344d' } } },
    yAxis: { type: 'value', axisLabel: { color: '#5f7fa6', fontSize: 10 }, splitLine: { lineStyle: { color: '#14273c' } } },
    series: rows.slice(0, 5).map(r => ({
      name: Object.entries(r.metric || {}).filter(([k]) => k !== '__name__')
        .map(([k, v]) => k + '=' + v).join(' ') || '合计',
      type: 'line', showSymbol: false, smooth: true,
      lineStyle: { color, width: 1.8 }, itemStyle: { color },
      data: (r.values || []).map(v => [v[0] * 1000, parseFloat(v[1])]),
    })),
  }
}

async function load() {
  try {
    overview.value = await obsOverview()
    const end = Date.now()
    const start = end - 3600 * 1000
    const step = 60
    const q = (t) => obsQuery({ template: t, start: new Date(start).toISOString(), end: new Date(end).toISOString(), step_seconds: step })
    const [errRes, p99Res] = await Promise.all([q('http_5xx_rate'), q('http_latency_p99')])
    await nextTick()
    if (errChart.value) (charts.find(c => c.el === errChart.value)?.inst || initChart(errChart.value))
      .setOption(lineOption(errRes.data, '#ff5c6c'), true)
    if (p99Chart.value) (charts.find(c => c.el === p99Chart.value)?.inst || initChart(p99Chart.value))
      .setOption(lineOption(p99Res.data, '#4da3ff'), true)
    // Warning 事件滚动条
    const ev = await listK8sEvents('', 60)
    warnings.value = (ev.data?.events || []).filter(e => e.type === 'Warning').slice(0, 8)
  } catch { /* 大屏容错：单次刷新失败保留上一帧 */ }
}

function tickClock() {
  clock.value = new Date().toLocaleTimeString('zh-CN', { hour12: false })
}

onMounted(() => {
  tickClock()
  load()
  timer = setInterval(load, 10000)
  clockTimer = setInterval(tickClock, 1000)
})
onUnmounted(() => {
  clearInterval(timer)
  clearInterval(clockTimer)
  charts.forEach(c => c.inst.dispose())
})
</script>

<template>
  <div class="noc">
    <header class="noc-head">
      <div class="noc-brand">AIOps · NOC 运维指挥</div>
      <div class="noc-clock">{{ clock }}</div>
      <div class="noc-live"><span class="pulse" /> LIVE</div>
      <router-link to="/" class="noc-back" title="返回控制台">← 控制台</router-link>
    </header>

    <div class="noc-stats">
      <div class="ncell">
        <div class="nlabel">服务实例</div>
        <div class="nval">{{ (overview?.services || []).length }}</div>
      </div>
      <div class="ncell" :class="{ danger: (overview?.err_5xx_rate || 0) > 0.05 }">
        <div class="nlabel">5xx 速率 (rps)</div>
        <div class="nval">{{ (overview?.err_5xx_rate ?? 0).toFixed(3) }}</div>
      </div>
      <div class="ncell">
        <div class="nlabel">总请求速率</div>
        <div class="nval">{{ (overview?.total_rate ?? 0).toFixed(3) }}</div>
      </div>
      <div class="ncell">
        <div class="nlabel">P99 延迟 (s)</div>
        <div class="nval">{{ (overview?.p99_seconds ?? 0).toFixed(3) }}</div>
      </div>
    </div>

    <div class="noc-charts">
      <div class="nchart"><div class="ntitle">5xx 错误率 · 近 1h</div><div ref="errChart" class="nchart-body" /></div>
      <div class="nchart"><div class="ntitle">P99 延迟 · 近 1h</div><div ref="p99Chart" class="nchart-body" /></div>
    </div>

    <div class="noc-bottom">
      <div class="ncell wide">
        <div class="nlabel">服务存活</div>
        <div class="upgrid">
          <span v-for="s in overview?.services || []" :key="s.service" class="updot" :class="{ down: !s.up }">
            {{ s.service.replace('makejob-', '') }}
          </span>
        </div>
      </div>
      <div class="ncell wide">
        <div class="nlabel">⚠ Warning 事件（实时滚动）</div>
        <div class="warnlist">
          <div v-for="(w, i) in warnings" :key="i" class="warnrow">{{ w.time }} · {{ w.object }} · {{ w.reason }} · {{ w.message }}</div>
          <div v-if="!warnings.length" class="nowarn">近 1 小时无 Warning 事件 ✓</div>
        </div>
      </div>
    </div>
  </div>
</template>

<style>
.noc {
  min-height: 100vh; background: #0a1526; color: #cfe3ff;
  font-family: 'Inter', 'Segoe UI', 'Microsoft YaHei', sans-serif;
  padding: 18px 22px; display: flex; flex-direction: column; gap: 14px;
}
.noc-head { display: flex; align-items: center; gap: 18px; }
.noc-brand { font-size: 20px; font-weight: 700; color: #e8f2ff; letter-spacing: 2px; }
.noc-clock { font-family: ui-monospace, monospace; font-size: 22px; color: #4da3ff; }
.noc-live { font-size: 12px; font-weight: 700; color: #ff5c6c; letter-spacing: 1px; }
.noc-back {
  margin-left: auto; font-size: 12.5px; color: #8fb6e8; text-decoration: none;
  border: 1px solid #1d344d; border-radius: 7px; padding: 5px 14px;
  background: rgba(15, 31, 54, 0.6); transition: all 0.16s ease;
}
.noc-back:hover { color: #cfe3ff; border-color: #2c4a6e; background: #12233d; }
.pulse { display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: #ff5c6c; margin-right: 6px; animation: pulse 1.4s infinite; }
.noc-stats { display: grid; grid-template-columns: repeat(4, 1fr); gap: 14px; }
.ncell { background: #0f1f36; border: 1px solid #1d344d; border-radius: 12px; padding: 14px 18px; }
.ncell.wide { grid-column: span 2; }
.ncell.danger .nval { color: #ff5c6c; }
.nlabel { font-size: 11px; color: #5f7fa6; letter-spacing: 1px; margin-bottom: 6px; }
.nval { font-size: 30px; font-weight: 700; color: #e8f2ff; font-variant-numeric: tabular-nums; }
.noc-charts { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
.nchart { background: #0f1f36; border: 1px solid #1d344d; border-radius: 12px; padding: 12px 14px; }
.ntitle { font-size: 12px; color: #5f7fa6; margin-bottom: 4px; }
.nchart-body { height: 200px; }
.noc-bottom { display: grid; grid-template-columns: 1fr 1.2fr; gap: 14px; }
.upgrid { display: flex; flex-wrap: wrap; gap: 8px; }
.updot {
  font-size: 12px; padding: 4px 10px; border-radius: 8px; color: #7ee2a8;
  background: rgba(16, 185, 129, 0.1); border: 1px solid rgba(16, 185, 129, 0.3);
  animation: breathe 3s infinite;
}
.updot.down { color: #ff8a94; background: rgba(255, 92, 108, 0.1); border-color: rgba(255, 92, 108, 0.35); animation: none; }
@keyframes breathe { 50% { opacity: 0.55; } }
.warnlist { max-height: 130px; overflow: auto; }
.warnrow { font-size: 12px; color: #ffb4b8; padding: 3px 0; border-bottom: 1px dashed rgba(255, 92, 108, 0.15); }
.nowarn { font-size: 12px; color: #7ee2a8; }
@keyframes pulse { 50% { opacity: 0.4; } }
</style>
