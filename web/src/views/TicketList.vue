<script setup>
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { createTicket, listTickets, statusType, statusLabel, typeLabel, deleteTicket } from '../api'
import { ElMessage } from 'element-plus'
import SkeletonCard from '../components/SkeletonCard.vue'

const router = useRouter()
const items = ref([])
const loading = ref(false)
const dlg = ref(false)
const form = ref({ title: '', content: '', reporter: '' })

const stats = ref({ total: 0, resolved: 0, escalated: 0, other: 0 })

async function load() {
  loading.value = true
  try {
    const d = await listTickets()
    items.value = d.items || []
    const s = { total: items.value.length, resolved: 0, escalated: 0, other: 0 }
    for (const t of items.value) {
      if (t.status === 'resolved') s.resolved++
      else if (t.status === 'escalated_human') s.escalated++
      else s.other++
    }
    stats.value = s
  } finally {
    loading.value = false
  }
}

async function submit() {
  if (!form.value.title || !form.value.content) return
  const d = await createTicket(form.value)
  dlg.value = false
  form.value = { title: '', content: '', reporter: '' }
  await load()
  router.push(`/tickets/${d.id}`)
}

const delDlg = ref(false)
const delTarget = ref(null)
const deleting = ref(false)

function askDelete(row) {
  delTarget.value = row
  delDlg.value = true
}

async function confirmDelete() {
  deleting.value = true
  try {
    await deleteTicket(delTarget.value.id)
    ElMessage.success('工单已删除')
    delDlg.value = false
    await load()
  } catch (e) {
    ElMessage.error(e.response?.data?.error || '删除失败')
  } finally {
    deleting.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="stat-grid">
    <div class="stat-card">
      <div><div class="stat-value">{{ stats.total }}</div><div class="stat-label">工单总数</div></div>
    </div>
    <div class="stat-card">
      <div><div class="stat-value">{{ stats.resolved }}</div><div class="stat-label">已解决</div></div>
    </div>
    <div class="stat-card">
      <div><div class="stat-value">{{ stats.escalated }}</div><div class="stat-label">转人工</div></div>
    </div>
    <div class="stat-card">
      <div><div class="stat-value">{{ stats.other }}</div><div class="stat-label">处理中/其它</div></div>
    </div>
  </div>

  <div class="card">
    <div class="card-title" style="justify-content: space-between">
      <span>运维工单</span>
      <el-button type="primary" @click="dlg = true">提交工单
      </el-button>
    </div>

    <SkeletonCard v-if="loading" :rows="4" />
    <el-table v-else :data="items" @row-click="row => router.push(`/tickets/${row.id}`)" style="cursor: pointer">
      <el-table-column prop="id" label="工单号" width="200">
        <template #default="{ row }"><span class="mono">{{ row.id }}</span></template>
      </el-table-column>
      <el-table-column prop="title" label="标题" min-width="180" show-overflow-tooltip />
      <el-table-column prop="type" label="类型" width="130">
        <template #default="{ row }"><el-tag size="small" effect="plain">{{ typeLabel(row.type) }}</el-tag></template>
      </el-table-column>
      <el-table-column prop="status" label="状态" width="110">
        <template #default="{ row }">
          <el-tag size="small" :type="statusType(row.status)">{{ statusLabel(row.status) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="来源" width="90">
        <template #default="{ row }">
          <el-tag v-if="row.reporter === 'alertmanager'" size="small" type="danger" effect="dark">告警</el-tag>
          <span v-else class="muted">人工</span>
        </template>
      </el-table-column>
      <el-table-column prop="entities.service" label="涉及服务" width="110" />
      <el-table-column prop="report.confidence" label="置信度" width="90">
        <template #default="{ row }">
          <span v-if="row.report" class="mono">{{ (row.report.confidence * 100).toFixed(0) }}%</span>
          <span v-else class="muted">—</span>
        </template>
      </el-table-column>
      <el-table-column prop="created_at" label="创建时间" width="170">
        <template #default="{ row }"><span class="muted">{{ new Date(row.created_at).toLocaleString('zh-CN') }}</span></template>
      </el-table-column>
      <el-table-column label="操作" width="80" fixed="right">
        <template #default="{ row }">
          <el-button size="small" type="danger" text :disabled="row.status === 'investigating' || row.status === 'awaiting_info'"
                     :title="row.status === 'investigating' || row.status === 'awaiting_info' ? '处理中的工单不允许删除' : '删除工单'"
                     @click.stop="askDelete(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>
  </div>

  <el-dialog v-model="dlg" title="提交运维工单" width="520">
    <el-form label-position="top">
      <el-form-item label="标题" required>
        <el-input v-model="form.title" placeholder="如：接口 5xx 突增" />
      </el-form-item>
      <el-form-item label="现象描述" required>
        <el-input v-model="form.content" type="textarea" :rows="4"
          placeholder="哪个服务/接口、什么异常、何时开始…" />
      </el-form-item>
      <el-form-item label="提交人">
        <el-input v-model="form.reporter" placeholder="可留空" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="dlg = false">取消</el-button>
      <el-button type="primary" @click="submit">提交并跟踪处理</el-button>
    </template>
  </el-dialog>

  <el-dialog v-model="delDlg" title="删除工单" width="420">
    <p style="font-size: 13px">
      确定删除工单 <b class="mono">{{ delTarget?.id }}</b>（{{ delTarget?.title }}）？
      其消息流水、观察记录将一并删除，此操作不可恢复。
    </p>
    <template #footer>
      <el-button @click="delDlg = false">取消</el-button>
      <el-button type="danger" :loading="deleting" @click="confirmDelete">确认删除</el-button>
    </template>
  </el-dialog>
</template>
