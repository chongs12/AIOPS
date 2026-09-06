<script setup>
// 骨架屏卡片：加载态占位（行数/是否带图表条可选）。
defineProps({
  rows: { type: Number, default: 3 },
  chart: { type: Boolean, default: false },
})
</script>

<template>
  <div class="card sk">
    <div class="sk-line w30" />
    <div v-if="chart" class="sk-chart" />
    <div v-for="i in rows" :key="i" class="sk-line" :style="{ width: 90 - i * 12 + '%' }" />
  </div>
</template>

<style scoped>
.sk { min-height: 80px; }
.sk-line { height: 14px; border-radius: 7px; background: linear-gradient(90deg, #eef2f7 25%, #f8fafc 50%, #eef2f7 75%); background-size: 200% 100%; animation: sk 1.4s infinite; margin-bottom: 12px; }
.sk-line.w30 { width: 30%; }
.sk-chart { height: 180px; border-radius: 10px; background: linear-gradient(90deg, #eef2f7 25%, #f8fafc 50%, #eef2f7 75%); background-size: 200% 100%; animation: sk 1.4s infinite; margin-bottom: 14px; }
@keyframes sk { 0% { background-position: 200% 0; } 100% { background-position: -200% 0; } }
</style>
