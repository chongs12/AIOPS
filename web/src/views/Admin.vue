<script setup>
import { ref, onMounted } from 'vue'
import { listPrompts, updatePrompt, resetPrompt, searchCases, getProviderChain, setProviderChain } from '../api'

const scenes = ref([])
const backend = ref('')
const editing = ref(null) // 正在编辑的场景
const draft = ref('')
const caseQuery = ref('')
const caseResults = ref([])
const caseLoading = ref(false)
const chain = ref([])
const chainLoading = ref(false)

async function load() {
  const d = await listPrompts()
  scenes.value = d.items || []
}

const cfg = ref({ model: '', temperature: 0.2, max_tokens: 4096, timeout_seconds: 60 })

function edit(s) {
  editing.value = s.scene
  draft.value = s.prompt
  cfg.value = {
    model: s.config?.model || '',
    temperature: s.config?.temperature || 0.2,
    max_tokens: s.config?.max_tokens || 4096,
    timeout_seconds: s.config?.timeout_seconds || 60,
  }
}

async function save() {
  await updatePrompt(editing.value, draft.value)
  ElMessage.success('已保存，≤5s 生效')
  editing.value = null
  await load()
}

async function saveAll(scene) {
  await updatePrompt(scene, draft.value, {
    model: cfg.value.model, temperature: Number(cfg.value.temperature),
    max_tokens: Number(cfg.value.max_tokens), timeout_seconds: Number(cfg.value.timeout_seconds),
  })
  ElMessage.success('已保存（Prompt + 参数），≤5s 生效')
  editing.value = null
  await load()
}

async function reset(scene) {
  await resetPrompt(scene)
  ElMessage.success('已恢复内置模板')
  await load()
}

async function search() {
  if (!caseQuery.value.trim()) return
  caseLoading.value = true
  try {
    const d = await searchCases(caseQuery.value)
    backend.value = d.backend
    caseResults.value = d.items || []
  } finally {
    caseLoading.value = false
  }
}

import { ElMessage } from 'element-plus'
const chainCols = [
  { key: 'name', label: '名称', ph: 'ark-deepseek' },
  { key: 'base_url', label: 'Base URL', ph: 'https://...' },
  { key: 'api_key', label: 'API Key', ph: 'ark-...' },
  { key: 'model', label: '模型', ph: 'deepseek-v4-flash' },
  { key: 'timeout_seconds', label: '超时s', ph: '120', num: true },
  { key: 'max_tokens', label: 'MaxTokens', ph: '4096', num: true },
  { key: 'temperature', label: '温度', ph: '0.2', num: true },
]

async function loadChain() {
  chainLoading.value = true
  try {
    const d = await getProviderChain()
    chain.value = (d.items || []).map(p => ({ ...p }))
  } finally {
    chainLoading.value = false
  }
}

function addProvider() {
  chain.value.push({ name: '', base_url: '', api_key: '', model: '',
    timeout_seconds: 300, max_tokens: 4096, temperature: 0.2 })
}

function removeProvider(i) {
  chain.value.splice(i, 1)
}

async function saveChain() {
  for (const p of chain.value) {
    if (!p.name || !p.base_url || !p.api_key || !p.model) {
      ElMessage.warning('每个 provider 的名称/BaseURL/Key/模型都必填')
      return
    }
  }
  await setProviderChain(chain.value)
  ElMessage.success('provider 链已保存，≤10s 内热替换生效（无需重启）')
}

onMounted(() => { load(); loadChain() })
</script>

<template>
  <div class="card">
    <div class="card-title">Prompt 管理<span class="muted">DB 外置 + 两级缓存（L1 5s / L2 60s）+ 版本失效，保存后 ≤5s 生效</span></div>
    <el-collapse>
      <el-collapse-item v-for="s in scenes" :key="s.scene" :name="s.scene">
        <template #title>
          <span class="mono" style="width: 110px">{{ s.scene }}</span>
          <el-tag size="small" :type="s.source === 'db' ? 'success' : 'info'">
            {{ s.source === 'db' ? 'DB 外置' : '内置' }}
          </el-tag>
          <span v-if="s.updated_at" class="muted" style="margin-left: 10px">更新于 {{ s.updated_at }}</span>
        </template>
        <el-input v-if="editing === s.scene" v-model="draft" type="textarea" :rows="14" />
        <pre v-else class="mono" style="white-space: pre-wrap; background: #f8fafc; padding: 12px; border-radius: 8px; font-size: 12px; line-height: 1.7">{{ s.prompt }}</pre>
        <div v-if="editing === s.scene" style="margin-top: 10px">
          <div class="muted" style="margin-bottom: 6px">场景模型参数（≤5s 生效；留空用 provider 默认）</div>
          <div style="display: flex; gap: 8px; flex-wrap: wrap">
            <el-input v-model="cfg.model" placeholder="模型名，如 glm-4.5" style="width: 220px" />
            <el-input-number v-model="cfg.temperature" :min="0" :max="2" :step="0.1" placeholder="温度" />
            <el-input-number v-model="cfg.max_tokens" :min="256" :max="32768" :step="256" placeholder="MaxTokens" />
            <el-input-number v-model="cfg.timeout_seconds" :min="10" :max="300" :step="10" placeholder="超时s" />
          </div>
          <div style="margin-top: 10px; display: flex; gap: 8px">
            <el-button type="primary" @click="saveAll(s.scene)">保存 Prompt + 参数</el-button>
            <el-button @click="editing = null">取消</el-button>
          </div>
        </div>
        <div v-else-if="s.config" class="muted" style="margin-top: 6px">
          参数: model={{ s.config.model || '默认' }} temp={{ s.config.temperature || '默认' }}
          max_tokens={{ s.config.max_tokens || '默认' }}
        </div>
        <div style="margin-top: 10px; display: flex; gap: 8px" v-if="editing !== s.scene">
          <el-button type="primary" plain @click="edit(s)">编辑</el-button>
          <el-button v-if="s.source === 'db'" @click="reset(s.scene)">恢复内置</el-button>
        </div>
      </el-collapse-item>
    </el-collapse>
  </div>

  <div class="card">
    <div class="card-title">Provider 链（热插拔）
      <span class="muted" style="margin-left: 8px">首个为主，失败自动切换 fallback；保存后 ≤10s 热替换生效，无需重启</span>
      <el-button size="small" style="margin-left: auto" @click="addProvider">＋ 添加</el-button>
      <el-button size="small" type="primary" :loading="chainLoading" @click="saveChain">保存并热替换</el-button>
    </div>
    <el-empty v-if="!chain.length" description="暂无自定义链（使用启动配置）" :image-size="50" />
    <div v-for="(p, i) in chain" :key="i" class="provider-row">
      <el-input v-model="p.name" placeholder="名称" style="width: 130px" />
      <el-input v-model="p.base_url" placeholder="Base URL" style="width: 280px" />
      <el-input v-model="p.api_key" placeholder="API Key" show-password style="width: 230px" />
      <el-input v-model="p.model" placeholder="模型名" style="width: 160px" />
      <el-input-number v-model="p.timeout_seconds" :min="10" :max="600" :step="10" controls-position="right" style="width: 100px" />
      <el-input-number v-model="p.max_tokens" :min="256" :max="65536" :step="256" controls-position="right" style="width: 110px" />
      <el-input-number v-model="p.temperature" :min="0" :max="2" :step="0.1" controls-position="right" style="width: 90px" />
      <el-button size="small" type="danger" text @click="removeProvider(i)">删除</el-button>
      <span v-if="i === 0" class="muted" style="font-size: 11px">← 主 provider</span>
    </div>
  </div>

  <div class="card">
    <div class="card-title">案例库检索
      <el-tag size="small" effect="plain">后端: {{ backend || '…' }}</el-tag>
      <span class="muted">已解决工单向量化沉淀（Milvus / PG 兜底）</span>
    </div>
    <div style="display: flex; gap: 10px; margin-bottom: 14px">
      <el-input v-model="caseQuery" placeholder="输入故障描述，检索相似历史处置…"
                @keyup.enter="search" />
      <el-button type="primary" :loading="caseLoading" @click="search">检索</el-button>
    </div>
    <el-empty v-if="!caseResults.length" description="暂无检索结果" :image-size="60" />
    <div v-for="c in caseResults" :key="c.ticket_id" class="report-card" style="margin-bottom: 10px">
      <div class="report-head">
        <span class="mono">{{ c.ticket_id }}</span>
        <el-tag size="small" effect="plain">{{ c.type }}</el-tag>
        <span class="conf-pill" style="font-size: 11px; padding: 1px 10px">相似度 {{ (c.score * 100).toFixed(0) }}%</span>
      </div>
      <div style="font-size: 13px; color: var(--text-secondary)">{{ c.root_cause || c.summary }}</div>
    </div>
  </div>
</template>
