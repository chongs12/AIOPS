<script setup>
import { ref, onMounted } from 'vue'
import { listDeployments, runK8sAction } from '../api'
import { ElMessage } from 'element-plus'

const items = ref([])
const loading = ref(false)
const dlg = ref(false)
const busy = ref(false)
const pending = ref({ name: '', action: '', replicas: 1 })

async function load() {
  loading.value = true
  try {
    const d = await listDeployments()
    items.value = d.data?.deployments || []
  } finally {
    loading.value = false
  }
}

function askScale(row) {
  pending.value = { name: row.name, action: 'k8s.scale', replicas: row.replicas }
  dlg.value = true
}

function askUndo(row) {
  pending.value = { name: row.name, action: 'k8s.rollout_undo', replicas: row.replicas }
  dlg.value = true
}

async function exec() {
  busy.value = true
  try {
    const input = pending.value.action === 'k8s.scale'
      ? { deployment: pending.value.name, replicas: Number(pending.value.replicas) }
      : { deployment: pending.value.name }
    const res = await runK8sAction(pending.value.action, input)
    ElMessage[res.status === 'ok' ? 'success' : 'warning'](
      res.status === 'ok' ? '已执行并写入审计' : `未执行: ${res.degraded_reason || ''}`)
    dlg.value = false
    await load()
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="card">
    <div class="card-title" style="justify-content: space-between">
      <span>🚀 部署管理
        <span class="muted" style="margin-left: 8px">扩缩容/回滚走确认门 + 审计落库（tool_audit_log）</span>
      </span>
      <el-button :loading="loading" @click="load">刷新</el-button>
    </div>

    <el-table :data="items" v-loading="loading" size="small">
      <el-table-column prop="name" label="Deployment" min-width="200">
        <template #default="{ row }"><span class="mono">{{ row.name }}</span></template>
      </el-table-column>
      <el-table-column label="副本" width="120">
        <template #default="{ row }">
          <el-tag size="small" :type="row.ready === row.replicas ? 'success' : 'warning'">
            {{ row.ready }}/{{ row.replicas }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="image" label="镜像" min-width="260" show-overflow-tooltip />
      <el-table-column prop="revision" label="Revision" width="90" />
      <el-table-column label="操作" width="200">
        <template #default="{ row }">
          <el-button size="small" type="primary" plain @click="askScale(row)"> 扩缩容</el-button>
          <el-button size="small" type="warning" plain @click="askUndo(row)"> 回滚</el-button>
        </template>
      </el-table-column>
    </el-table>
  </div>

  <el-dialog v-model="dlg" :title="pending.action === 'k8s.scale' ? '扩缩容确认' : '回滚确认'" width="440">
    <p style="font-size: 13px; margin-bottom: 10px">
      对 <b class="mono">{{ pending.name }}</b>
      {{ pending.action === 'k8s.scale' ? '调整副本数' : '回滚到上一 revision' }}。
    </p>
    <el-form label-width="90px" v-if="pending.action === 'k8s.scale'">
      <el-form-item label="目标副本数">
        <el-input-number v-model="pending.replicas" :min="0" :max="10" />
      </el-form-item>
    </el-form>
    <el-alert type="warning" :closable="false" title="变更操作将写入审计日志（console）" />
    <template #footer>
      <el-button @click="dlg = false">取消</el-button>
      <el-button type="primary" :loading="busy" @click="exec">确认执行</el-button>
    </template>
  </el-dialog>
</template>
