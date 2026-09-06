<script setup>
// 知识库：案例沉淀浏览 + 相似检索。
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { listCases, searchCases, caseDetail, typeLabel } from '../api'

const router = useRouter()
const items = ref([])
const loading = ref(false)
const q = ref('')
const results = ref([])
const searching = ref(false)
const drawer = ref(false)
const detail = ref(null)
const backend = ref('')

async function load() {
  loading.value = true
  try {
    const d = await listCases(30)
    items.value = d.items || []
  } finally {
    loading.value = false
  }
}

async function search() {
  if (!q.value.trim()) return
  searching.value = true
  try {
    const d = await searchCases(q.value)
    backend.value = d.backend || ''
    results.value = d.items || []
  } finally {
    searching.value = false
  }
}

async function openDetail(ticketId) {
  detail.value = await caseDetail(ticketId)
  drawer.value = true
}

onMounted(load)
</script>

<template>
  <div class="card">
    <div class="card-title">📚 知识库（已解决工单案例沉淀）
      <span class="muted" style="margin-left: 8px">Milvus 向量检索 / PG 兜底</span>
    </div>
    <div style="display: flex; gap: 10px; margin-bottom: 14px">
      <el-input v-model="q" placeholder="检索相似历史处置…" @keyup.enter="search" />
      <el-button type="primary" :loading="searching" @click="search">检索</el-button>
    </div>
    <div v-if="results.length" style="margin-bottom: 14px">
      <div class="muted" style="margin-bottom: 8px">检索结果（后端: {{ backend }}）</div>
      <div v-for="r in results" :key="r.ticket_id" class="case-card" @click="openDetail(r.ticket_id)">
        <div style="display: flex; align-items: center; gap: 8px">
          <span class="mono">{{ r.ticket_id }}</span>
          <el-tag size="small" effect="plain">{{ typeLabel(r.type) }}</el-tag>
          <span class="conf-pill" style="font-size: 11px; padding: 1px 10px">相似度 {{ (r.score * 100).toFixed(0) }}%</span>
        </div>
        <div style="font-size: 13px; color: var(--text-secondary); margin-top: 4px">{{ r.root_cause || r.summary }}</div>
      </div>
    </div>
  </div>

  <div class="card" v-loading="loading">
    <div class="card-title">🗂️ 全部案例</div>
    <el-empty v-if="!items.length && !loading" description="暂无沉淀案例——解决一张工单后自动入库" :image-size="60" />
    <div v-for="c in items" :key="c.ticket_id" class="case-card" @click="openDetail(c.ticket_id)">
      <div style="display: flex; align-items: center; gap: 8px">
        <span class="mono">{{ c.ticket_id }}</span>
        <el-tag size="small" effect="plain">{{ typeLabel(c.type) }}</el-tag>
        <span class="muted">{{ c.service }}</span>
        <span class="muted" style="margin-left: auto">{{ new Date(c.created_at).toLocaleDateString('zh-CN') }}</span>
      </div>
      <div style="font-size: 13px; margin-top: 4px">{{ c.root_cause || c.summary }}</div>
    </div>
  </div>

  <el-drawer v-model="drawer" size="520px" :title="detail?.title">
    <template v-if="detail">
      <div style="display: flex; gap: 8px; margin-bottom: 12px; align-items: center">
        <span class="mono">{{ detail.ticket_id }}</span>
        <el-tag size="small" effect="plain">{{ typeLabel(detail.type) }}</el-tag>
        <el-tag size="small" :type="detail.status === 'resolved' ? 'success' : 'info'">{{ detail.status }}</el-tag>
      </div>
      <template v-if="detail.report">
        <h4 style="font-size: 12px; color: var(--text-secondary)">根因</h4>
        <div style="font-size: 13px; margin-bottom: 10px">{{ detail.report.root_cause }}</div>
        <h4 style="font-size: 12px; color: var(--text-secondary)">摘要</h4>
        <div style="font-size: 13px; margin-bottom: 10px">{{ detail.report.summary }}</div>
        <div v-if="detail.report.recommendations?.length">
          <h4 style="font-size: 12px; color: var(--text-secondary)">处置建议</h4>
          <ul style="margin-left: 18px; font-size: 13px">
            <li v-for="rec in detail.report.recommendations" :key="rec.description">{{ rec.description }}</li>
          </ul>
        </div>
      </template>
      <router-link :to="`/tickets/${detail.ticket_id}`">
        <el-button type="primary" plain style="margin-top: 12px">查看完整工单 →</el-button>
      </router-link>
    </template>
  </el-drawer>
</template>

<style scoped>
.case-card {
  background: #f8fafc; border: 1px solid rgba(148, 163, 184, 0.16); border-radius: 10px;
  padding: 10px 14px; margin-bottom: 8px; cursor: pointer; transition: all 0.15s ease;
}
.case-card:hover { transform: translateY(-1px); box-shadow: 0 6px 16px rgba(15, 23, 42, 0.08); background: #fff; }
</style>

__zcode_status=$?
if [ "$__zcode_status" -eq 0 ]; then pwd -P > '/c/Users/Lenovo/AppData/Local/Temp/zcode-ccb1d7ba-1045-44ef-b6aa-9c8e478d286b-cwd'; fi
exit "$__zcode_status"
