<script setup>
import { useRoute } from 'vue-router'
import { computed } from 'vue'
import NocScreen from './views/NocScreen.vue'

const route = useRoute()
const noChrome = () => route.meta?.chrome === false
const pageTitle = computed(() => route.meta?.title || '')
</script>

<template>
  <NocScreen v-if="route.meta?.chrome === false" />
  <div v-else class="app-layout">
    <aside class="sidebar">
      <div class="sidebar-logo">
        <div class="logo-icon">AI</div>
        <div class="logo-copy">
          <span class="logo-text">AIOps</span>
          <span class="logo-subtext">运维智能体平台</span>
        </div>
      </div>

      <el-menu router :default-active="route.path" class="sidebar-nav">
        <el-menu-item-group title="智能运维">
          <el-menu-item index="/">
            <el-icon><ChatDotRound /></el-icon><span>AI 对话</span>
          </el-menu-item>
          <el-menu-item index="/tickets">
            <el-icon><Odometer /></el-icon><span>工作台</span>
          </el-menu-item>
        </el-menu-item-group>
        <el-menu-item-group title="可观测">
          <el-menu-item index="/observability">
            <el-icon><DataLine /></el-icon><span>总览</span>
          </el-menu-item>
          <el-menu-item index="/metrics">
            <el-icon><TrendCharts /></el-icon><span>指标查询</span>
          </el-menu-item>
          <el-menu-item index="/logs">
            <el-icon><Document /></el-icon><span>日志查询</span>
          </el-menu-item>
          <el-menu-item index="/events">
            <el-icon><Bell /></el-icon><span>事件墙</span>
          </el-menu-item>
          <el-menu-item index="/trace">
            <el-icon><Connection /></el-icon><span>链路追踪</span>
          </el-menu-item>
        </el-menu-item-group>
        <el-menu-item-group title="平台管理">
          <el-menu-item index="/deployments">
            <el-icon><Box /></el-icon><span>部署管理</span>
          </el-menu-item>
          <el-menu-item index="/knowledge">
            <el-icon><Collection /></el-icon><span>知识库</span>
          </el-menu-item>
          <el-menu-item index="/admin">
            <el-icon><Setting /></el-icon><span>管理面</span>
          </el-menu-item>
        </el-menu-item-group>
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
          <router-link to="/noc" style="text-decoration: none">
            <el-button size="small" plain>🖥️ NOC 大屏</el-button>
          </router-link>
          <span class="model-badge">🤖 deepseek-v4-flash</span>
        </div>
      </header>
      <main class="content">
        <router-view />
      </main>
    </div>
  </div>
</template>
