<script setup>
// 置信度仪表环：SVG 圆环 + 百分比动画（绿≥0.6 / 黄 0.35–0.6 / 红<0.35）。
import { ref, computed, onMounted, watch } from 'vue'

const props = defineProps({
  confidence: { type: Number, required: true }, // 0-1
  size: { type: Number, default: 64 },
})

const pct = computed(() => Math.round(Math.min(1, Math.max(0, props.confidence)) * 100))
const color = computed(() =>
  props.confidence >= 0.6 ? '#10b981' : props.confidence >= 0.35 ? '#f59e0b' : '#ef4444')
const R = computed(() => (props.size - 8) / 2)
const C = computed(() => 2 * Math.PI * R.value)

const shown = ref(0)
onMounted(animate)
watch(() => props.confidence, animate)

function animate() {
  const target = pct.value
  const t0 = performance.now()
  const step = (t) => {
    const p = Math.min(1, (t - t0) / 600)
    shown.value = Math.round(target * (1 - Math.pow(1 - p, 3))) // easeOutCubic
    if (p < 1) requestAnimationFrame(step)
  }
  requestAnimationFrame(step)
}
</script>

<template>
  <div class="gauge" :style="{ width: size + 'px', height: size + 'px' }" :title="`置信度 ${pct}%`">
    <svg :width="size" :height="size">
      <circle :cx="size / 2" :cy="size / 2" :r="R" fill="none" stroke="#eef2f7" stroke-width="6" />
      <circle :cx="size / 2" :cy="size / 2" :r="R" fill="none" :stroke="color" stroke-width="6"
              stroke-linecap="round" :stroke-dasharray="C"
              :stroke-dashoffset="C * (1 - shown / 100)"
              :transform="`rotate(-90 ${size / 2} ${size / 2})`"
              style="transition: stroke-dashoffset 0.1s linear" />
    </svg>
    <div class="gauge-num" :style="{ color }">{{ shown }}<span class="gauge-pct">%</span></div>
  </div>
</template>

<style scoped>
.gauge { position: relative; display: inline-flex; align-items: center; justify-content: center; }
.gauge-num { position: absolute; font-weight: 700; font-size: 14px; }
.gauge-pct { font-size: 9px; font-weight: 600; margin-left: 1px; }
svg { display: block; }
</style>
