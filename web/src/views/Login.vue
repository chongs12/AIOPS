<script setup>
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import axios from 'axios'
import { ElMessage } from 'element-plus'

const route = useRoute()
const router = useRouter()
const form = ref({ username: 'admin', password: '' })
const loading = ref(false)

async function login() {
  loading.value = true
  try {
    const { data } = await axios.post('/api/v1/auth/login', form.value)
    localStorage.setItem('aiops_token', data.token)
    ElMessage.success('登录成功')
    router.push(route.query.next ? String(route.query.next) : '/')
  } catch (e) {
    ElMessage.error(e.response?.data?.error || '登录失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div style="min-height: 100vh; display: flex; align-items: center; justify-content: center; background: var(--content-bg)">
    <div class="card" style="width: 380px; padding: 28px">
      <div style="display: flex; align-items: center; gap: 10px; margin-bottom: 18px">
        <div class="logo-icon">AI</div>
        <div><div class="logo-text">AIOps 登录</div><div class="muted">管理面与变更操作需要登录</div></div>
      </div>
      <el-form label-position="top" @keyup.enter="login">
        <el-form-item label="账号"><el-input v-model="form.username" /></el-form-item>
        <el-form-item label="密码"><el-input v-model="form.password" type="password" show-password /></el-form-item>
        <el-button type="primary" style="width: 100%" :loading="loading" @click="login">登 录</el-button>
      </el-form>
    </div>
  </div>
</template>
