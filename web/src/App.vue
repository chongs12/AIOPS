<script setup>
import { useRoute } from 'vue-router'
import { computed } from 'vue'

const route = useRoute()
const pageTitle = computed(() => route.meta?.title || '')
</script>

<template>
  <div class="app-layout">
    <aside class="sidebar">
      <div class="sidebar-logo">
        <div class="logo-icon">AI</div>
        <div class="logo-copy">
          <span class="logo-text">AIOps</span>
          <span class="logo-subtext">运维智能体平台</span>
        </div>
      </div>

      <el-menu router :default-active="route.path" class="sidebar-nav">
        <el-menu-item index="/">
          <el-icon><ChatDotRound /></el-icon>
          <span>AI 对话</span>
        </el-menu-item>
        <el-menu-item index="/tickets">
          <el-icon><Odometer /></el-icon>
          <span>工作台</span>
        </el-menu-item>
        <el-menu-item index="/admin">
          <el-icon><Setting /></el-icon>
          <span>管理面</span>
        </el-menu-item>
      </el-menu>

      <div class="sidebar-footer">多 Agent 证据化推理<br>工具降级 · 人工确认</div>
    </aside>

    <div class="main-area">
      <header class="topbar">
        <div>
          <div class="topbar-title">{{ pageTitle }}</div>
          <div class="topbar-sub">AIOps 运维工单多智能体系统 · 数据源为真实集群</div>
        </div>
        <div class="topbar-right">
          <span class="model-badge">🤖 deepseek-v4-flash</span>
        </div>
      </header>
      <main class="content">
        <router-view />
      </main>
    </div>
  </div>
</template>
