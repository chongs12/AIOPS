<script setup>
import { ref, onMounted } from 'vue'
import { jaegerServices, jaegerTraces } from '../api'

const services = ref([])
const cur = ref('')
const traces = ref([])
const loading = ref(false)
const iframeUrl = ref('')
const showIframe = ref(false)

async function loadServices() {
  const d = await jaegerServices()
  services.value = d.data || []
  if (services.value.length) select(services.value[0])
}

async function select(svc) {
  cur.value = svc
  loading.value = true
  try {
    const d = await jaegerTraces(svc)
    // Jaeger /api/traces 响应: {data: [{traceID, spans: [{startTime(µs), duration(µs)}]}]}
    const list = Array.isArray(d.data) ? d.data : []
    traces.value = list.map(t => {
      const spans = t.spans || []
      const start = Math.min(...spans.map(s => s.startTime))
      const end = Math.max(...spans.map(s => s.startTime + s.duration))
      return { traceID: t.traceID, spans: spans.length, durationUs: end - start, start }
    }).sort((a, b2) => b2.start - a.start)
  } finally {
    loading.value = false
  }
}

const fmtDur = (us) => us >= 1000 ? (us / 1000).toFixed(1) + 'ms' : us + 'µs'
const fmtStart = (us) => new Date(us / 1000).toLocaleString('zh-CN')

onMounted(loadServices)
</script>

<template>
  <div class="card">
    <div class="card-title">🔗 链路追踪（Jaeger）
      <span class="muted" style="margin-left: 8px">数据源: jaeger.localhost 查询 API</span>
    </div>
    <div style="display: flex; gap: 10px; flex-wrap: wrap; margin-bottom: 14px">
      <el-select v-model="cur" style="width: 260px" @change="select">
        <el-option v-for="s in services" :key="s" :label="s" :value="s" />
      </el-select>
      <el-button :loading="loading" @click="select(cur)">刷新</el-button>
    </div>

    <el-table :data="traces" size="small" v-loading="loading">
      <el-table-column prop="traceID" label="Trace ID" min-width="260">
        <template #default="{ row }"><span class="mono">{{ row.traceID }}</span></template>
      </el-table-column>
      <el-table-column label="Span 数" width="100">
        <template #default="{ row }">{{ row.spans }}</template>
      </el-table-column>
      <el-table-column label="耗时" width="110">
        <template #default="{ row }">{{ fmtDur(row.durationUs) }}</template>
      </el-table-column>
      <el-table-column label="开始时间" width="180">
        <template #default="{ row }">{{ fmtStart(row.start) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="180">
        <template #default="{ row }">
          <a :href="`http://jaeger.localhost:8080/trace/${row.traceID}`" target="_blank">
            <el-button size="small" plain>在 Jaeger 打开</el-button>
          </a>
        </template>
      </el-table-column>
    </el-table>
    <div v-if="!traces.length && !loading" class="muted" style="padding: 12px">
      近 1 小时该服务无 trace（提交一张工单即可产生完整推理链路）
    </div>
  </div>
</template>
