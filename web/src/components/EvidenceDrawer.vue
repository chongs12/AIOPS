<script setup>
// 证据抽屉：点击任意 obs-xxx 徽标 → 加载完整观察（含原始 data）并侧滑展示。
import { ref, watch } from 'vue'
import axios from 'axios'
import { ElMessage } from 'element-plus'

const props = defineProps({
  ticketId: { type: String, required: true },
})
const visible = ref(false)
const loading = ref(false)
const obs = ref(null)
let currentId = ''

// 供父组件调用的打开方法（也可用 v-model:obs-id）。
async function open(obsId) {
  currentId = obsId
  visible.value = true
  loading.value = true
  obs.value = null
  try {
    const { data } = await axios.get(
      `/api/v1/tickets/${props.ticketId}/observations/${obsId}`)
    if (currentId !== obsId) return // 快速连点防护
    obs.value = data
  } catch (e) {
    ElMessage.error(e.response?.data?.error || '观察加载失败')
    visible.value = false
  } finally {
    loading.value = false
  }
}
defineExpose({ open })

const prettyData = () => {
  if (!obs.value?.data) return null
  try {
    return JSON.stringify(obs.value.data, null, 2)
  } catch {
    return String(obs.value.data)
  }
}

const copied = ref(false)
async function copyData() {
  const text = prettyData()
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    copied.value = true
    setTimeout(() => (copied.value = false), 1500)
  } catch { /* 剪贴板不可用则忽略 */ }
}
</script>

<template>
  <el-drawer v-model="visible" size="520px" :with-header="true" destroy-on-close>
    <template #header>
      <div style="display: flex; align-items: center; gap: 8px">
        <span class="mono" style="font-weight: 700">{{ obs?.id || '…' }}</span>
        <el-tag v-if="obs" size="small" :type="obs.status === 'ok' ? 'success' : 'danger'">{{ obs.status }}</el-tag>
      </div>
    </template>
    <div v-loading="loading">
      <template v-if="obs">
        <div class="kv"><span class="k">来源</span><span class="mono">{{ obs.source }}</span></div>
        <div class="kv"><span class="k">时间</span><span class="mono">{{ new Date(obs.created_at).toLocaleString('zh-CN') }}</span></div>
        <div class="kv col"><span class="k">查询</span>
          <div class="code-block mono">{{ obs.query || '—' }}</div>
        </div>
        <div class="kv col"><span class="k">摘要</span><div style="font-size: 13px; line-height: 1.6">{{ obs.summary }}</div></div>
        <div class="kv col" v-if="prettyData()">
          <div style="display: flex; align-items: center; justify-content: space-between; width: 100%">
            <span class="k">原始数据</span>
            <el-button size="small" text type="primary" @click="copyData">
              {{ copied ? '✓ 已复制' : '复制' }}
            </el-button>
          </div>
          <pre class="code-block mono data-block">{{ prettyData() }}</pre>
        </div>
      </template>
    </div>
  </el-drawer>
</template>

<style scoped>
.kv { display: flex; gap: 10px; margin-bottom: 12px; align-items: baseline; }
.kv.col { flex-direction: column; gap: 4px; }
.k { font-size: 11px; color: #94a3b8; font-weight: 600; flex-shrink: 0; width: 40px; }
.code-block {
  background: #0f172a; color: #cbd5e1; border-radius: 8px; padding: 10px 12px;
  font-size: 12px; line-height: 1.6; word-break: break-all; white-space: pre-wrap;
}
.data-block { max-height: 320px; overflow: auto; }
.mono { font-family: ui-monospace, monospace; }
</style>
