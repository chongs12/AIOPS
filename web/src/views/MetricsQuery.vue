<script setup>
import { ref, onMounted, onUnmounted, nextTick } from 'vue'
import echarts from '../lib/echarts'
import { listObsTemplates, obsQuery } from '../api'
import { ElMessage } from 'element-plus'

const templates = ref([])
const cur = ref(null)
const windowVal = ref('5m')
const rangeMin = ref(60)
const freeMode = ref(false)
const freeExpr = ref('sum(rate(http_requests_total{code=~"5.."}[5m]))')
const loading = ref(false)
const lastRes = ref(null)
const chartEl = ref(null)
let chart = null

const quickRanges = [
  { label: '近 15 分钟', value: 15 },
  { label: '近 1 小时', value: 60 },
  { label: '近 6 小时', value: 360 },
]

async function loadTemplates() {
  const d = await listObsTemplates()
  templates.value = d.items || []
  if (templates.value.length) select(templates.value[0])
}

function select(t) {
  cur.value = t
  // 参数缺省值填充
  for (const p of t.params || []) {
    if (p.default) p._val = p.default
  }
}

async function run() {
  loading.value = true
  try {
    const end = Date.now()
    const start = end - rangeMin.value * 60 * 1000
    const step = Math.max(15, Math.floor(rangeMin.value * 60 / 120))
    let payload
    if (freeMode.value) {
      payload = { mode: 'free', expr: freeExpr.value, start: new Date(start).toISOString(), end: new Date(end).toISOString(), step_seconds: step }
    } else {
      const params = {}
      for (const p of cur.value?.params || []) {
        if (p._val) params[p.name] = p._val
      }
      params.window = params.window || windowVal.value
      payload = { template: cur.value.name, params, start: new Date(start).toISOString(), end: new Date(end).toISOString(), step_seconds: step }
    }
    lastRes.value = await obsQuery(payload)
    if (lastRes.value.status !== 'ok') {
      ElMessage.warning(lastRes.value.degraded_reason || '查询失败')
    }
    render()
  } finally {
    loading.value = false
  }
}

const seriesRows = () => {
  if (!lastRes.value || lastRes.value.status !== 'ok') return []
  return lastRes.value.data || []
}

function render() {
  nextTick(() => {
    if (!chartEl.value) return
    if (!chart) chart = echarts.init(chartEl.value)
    const rows = seriesRows().slice(0, 8)
    const series = rows.map((r, i) => ({
      name: nameOf(r.metric, i),
      type: 'line', showSymbol: false, smooth: true,
      data: (r.values || []).map(v => [v[0] * 1000, parseFloat(v[1])]),
    }))
    chart.setOption({
      grid: { left: 56, right: 16, top: 32, bottom: 28 },
      tooltip: { trigger: 'axis' },
      legend: { show: series.length > 1, top: 0, textStyle: { fontSize: 11 } },
      xAxis: { type: 'time', axisLabel: { fontSize: 10 } },
      yAxis: { type: 'value', axisLabel: { fontSize: 10 }, splitLine: { lineStyle: { color: '#eef2f7' } } },
      color: ['#6366f1', '#ef4444', '#10b981', '#f59e0b', '#3b82f6', '#8b5cf6', '#ec4899', '#64748b'],
      series,
    }, true)
  })
}

function nameOf(metric, i) {
  if (!metric || Object.keys(metric).length === 0) return '序列 ' + (i + 1)
  return Object.entries(metric).filter(([k]) => k !== '__name__')
    .map(([k, v]) => `${k}=${v}`).join(' ') || '合计'
}

onMounted(loadTemplates)
onUnmounted(() => chart && chart.dispose())
</script>

<template>
  <div class="card">
    <div class="card-title">🔎 指标查询
      <el-switch v-model="freeMode" active-text="自由 PromQL（白名单受限）" style="margin-left: auto" />
    </div>

    <div style="display: flex; gap: 12px; flex-wrap: wrap; align-items: center; margin-bottom: 14px">
      <template v-if="!freeMode">
        <el-select v-model="cur" value-key="name" placeholder="选择查询模板" style="width: 300px" @change="select">
          <el-option v-for="t in templates" :key="t.name" :label="`${t.name} — ${t.desc}`" :value="t" />
        </el-select>
        <el-input v-model="windowVal" style="width: 90px" placeholder="window" />
      </template>
      <template v-else>
        <el-input v-model="freeExpr" style="flex: 1; min-width: 320px"
                  placeholder='如 sum(rate(http_requests_total{code=~"5.."}[5m]))' />
      </template>
      <el-select v-model="rangeMin" style="width: 120px">
        <el-option v-for="r in quickRanges" :key="r.value" :label="r.label" :value="r.value" />
      </el-select>
      <el-button type="primary" :loading="loading" @click="run">查询</el-button>
    </div>

    <!-- 模板说明与参数 -->
    <el-alert v-if="cur && !freeMode" type="info" :closable="false" style="margin-bottom: 12px">
      <div class="mono" style="font-size: 12px">{{ cur.expr }}</div>
      <div class="muted" style="margin-top: 4px">参数：{{ (cur.params || []).map(p => p.name + (p.default ? '=' + p.default : '')).join('、') || '无' }}</div>
    </el-alert>

    <!-- 结果状态 pills -->
    <div v-if="lastRes" style="display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 12px">
      <span class="query-pill" :class="lastRes.status === 'ok' ? 'ok' : 'bad'">{{ lastRes.status }}</span>
      <span class="query-pill">序列数: {{ seriesRows().length }}</span>
      <span class="query-pill">耗时: {{ lastRes.duration_ms }}ms</span>
      <span v-if="lastRes.from_cache" class="query-pill">缓存命中</span>
      <span class="query-pill mono">{{ lastRes.evidence?.query }}</span>
    </div>

    <div ref="chartEl" style="height: 300px"></div>

    <!-- 数据表 -->
    <el-table v-if="seriesRows().length" :data="seriesRows()" size="small" style="margin-top: 12px">
      <el-table-column label="序列标签" min-width="220">
        <template #default="{ row }"><span class="mono">{{ nameOf(row.metric, 0) }}</span></template>
      </el-table-column>
      <el-table-column label="点数" width="80">
        <template #default="{ row }">{{ (row.values || []).length }}</template>
      </el-table-column>
      <el-table-column label="最新值" width="110">
        <template #default="{ row }">
          {{ (row.values || []).length ? parseFloat(row.values[row.values.length - 1][1]).toFixed(4) : '—' }}
        </template>
      </el-table-column>
    </el-table>
  </div>
</template>

<style scoped>
.query-pill {
  font-size: 11px; padding: 3px 10px; border-radius: 999px;
  background: #f1f5f9; color: #475569;
}
.query-pill.ok { background: #ecfdf5; color: #047857; }
.query-pill.bad { background: #fef2f2; color: #b91c1c; }
</style>
