<script setup>
import { ref, computed, onMounted } from 'vue'
import { queryK8sLogs, KNOWN_SERVICES } from '../api'

const service = ref('gateway')
const since = ref(60)
const tail = ref(200)
const keyword = ref('')
const pods = ref([])
const loading = ref(false)
const lastQuery = ref('')

async function load() {
  loading.value = true
  try {
    const d = await queryK8sLogs(service.value === '__all__' ? '' : 'makejob-' + service.value, since.value, tail.value)
    pods.value = d.data?.pods || []
    lastQuery.value = d.evidence?.query || ''
  } finally {
    loading.value = false
  }
}

const isError = (line) => /error|ERROR|panic|FATAL|failed|Failed|timeout|refused|OOM/i.test(line)
const filtered = (logs) => {
  const lines = (logs || '').split('\n')
  if (!keyword.value.trim()) return lines
  const k = keyword.value.toLowerCase()
  return lines.filter(l => l.toLowerCase().includes(k))
}

onMounted(load)
</script>

<template>
  <div class="card">
    <div class="card-title">📜 容器日志查询
      <span class="muted" style="margin-left: 8px">数据源: client-go 直拉（k8s.logs 工具管道）</span>
    </div>
    <div style="display: flex; gap: 10px; flex-wrap: wrap; margin-bottom: 12px">
      <el-select v-model="service" style="width: 180px">
        <el-option label="全部服务" value="__all__" />
        <el-option v-for="s in KNOWN_SERVICES" :key="s" :label="s" :value="s" />
      </el-select>
      <el-select v-model="since" style="width: 130px">
        <el-option :value="30" label="近 30 分钟" />
        <el-option :value="60" label="近 1 小时" />
        <el-option :value="360" label="近 6 小时" />
      </el-select>
      <el-select v-model="tail" style="width: 120px">
        <el-option :value="100" label="尾部 100 行" />
        <el-option :value="200" label="尾部 200 行" />
        <el-option :value="500" label="尾部 500 行" />
      </el-select>
      <el-button type="primary" :loading="loading" @click="load">查询</el-button>
    </div>
    <div class="mono muted" v-if="lastQuery">{{ lastQuery }}</div>
  </div>

  <div v-for="p in pods" :key="p.pod" class="card">
    <div class="card-title" style="justify-content: space-between">
      <span class="mono">{{ p.pod }}</span>
      <span class="muted">{{ p.bytes }} 字节</span>
    </div>
    <div class="log-viewer">
      <div v-for="(line, i) in filtered(p.logs)" :key="i"
           class="log-line" :class="{ err: isError(line) }">
        <span class="log-no">{{ i + 1 }}</span>{{ line }}
      </div>
      <div v-if="!filtered(p.logs).length" class="muted" style="padding: 10px">无匹配日志行</div>
    </div>
  </div>
</template>

<style scoped>
.log-viewer {
  background: #0f172a; border-radius: 8px; padding: 10px 0;
  max-height: 420px; overflow: auto; font-family: ui-monospace, Consolas, monospace; font-size: 12px;
}
.log-line { color: #cbd5e1; padding: 1px 14px; white-space: pre-wrap; word-break: break-all; line-height: 1.5; }
.log-line.err { color: #f87171; background: rgba(239, 68, 68, 0.08); }
.log-no { display: inline-block; width: 42px; color: #475569; user-select: none; }
</style>
