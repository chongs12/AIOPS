<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import { listK8sEvents } from '../api'

const items = ref([])
const podPrefix = ref('')
const since = ref(60)
const loading = ref(false)
const lastAt = ref('')
let timer = null
const rowCls = ({ row }) => (row.type === 'Warning' ? 'warn-row' : '')

async function load() {
  loading.value = true
  try {
    const d = await listK8sEvents(podPrefix.value, since.value)
    items.value = d.data?.events || []
    lastAt.value = new Date().toLocaleTimeString('zh-CN')
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  timer = setInterval(load, 30000) // 30s 自动刷新（事件墙）
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div class="card">
    <div class="card-title" style="justify-content: space-between">
      <span>K8s 事件墙
        <span class="muted" style="margin-left: 8px">Warning 红色高亮 · 每 30s 自动刷新 · 最近刷新 {{ lastAt }}</span>
      </span>
      <div style="display: flex; gap: 8px">
        <el-input v-model="podPrefix" placeholder="Pod 前缀（可空）" style="width: 200px" clearable @keyup.enter="load" />
        <el-select v-model="since" style="width: 120px">
          <el-option :value="30" label="近 30 分钟" />
          <el-option :value="60" label="近 1 小时" />
          <el-option :value="360" label="近 6 小时" />
        </el-select>
        <el-button type="primary" :loading="loading" @click="load">刷新</el-button>
      </div>
    </div>

    <el-table :data="items" size="small" v-loading="loading" :row-class-name="rowCls">
      <el-table-column prop="time" label="时间" width="170" />
      <el-table-column prop="type" label="类型" width="90">
        <template #default="{ row }">
          <el-tag size="small" :type="row.type === 'Warning' ? 'danger' : 'info'">{{ row.type }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="reason" label="原因" width="140" />
      <el-table-column prop="object" label="对象" width="240" />
      <el-table-column prop="count" label="次数" width="70" />
      <el-table-column prop="message" label="消息" min-width="300" show-overflow-tooltip />
    </el-table>
    <div v-if="!items.length && !loading" class="muted" style="padding: 12px">时间窗内无事件（一切正常）</div>
  </div>
</template>


<style>
.el-table .warn-row { --el-table-tr-bg-color: #fff7f7; }
</style>
