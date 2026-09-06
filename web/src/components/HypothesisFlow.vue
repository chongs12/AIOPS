<script setup>
// 假设状态流：confirmed/rejected/pending 三色 + 置信度条 + 可点击证据徽标。
defineProps({
  hypotheses: { type: Array, default: () => [] }, // [{statement, confidence, status, evidence_ids}]
})
const emit = defineEmits(['open-evidence'])

const meta = {
  confirmed: { icon: 'CONFIRMED', label: '已证实', color: '#16875b', bg: 'rgba(22,135,91,0.07)' },
  rejected: { icon: 'REJECTED', label: '已排除', color: '#d64550', bg: 'rgba(214,69,80,0.06)' },
  pending: { icon: 'PENDING', label: '待验证', color: '#b45309', bg: 'rgba(180,83,9,0.06)' },
}
</script>

<template>
  <div class="hf" v-if="hypotheses.length">
    <div v-for="(h, i) in hypotheses" :key="i" class="hf-row"
         :style="{ background: (meta[h.status] || meta.pending).bg }">
      <div class="hf-icon" :style="{ color: (meta[h.status] || meta.pending).color }">
        {{ (meta[h.status] || meta.pending).icon }}
      </div>
      <div class="hf-main">
        <div class="hf-head">
          <span class="hf-label" :style="{ color: (meta[h.status] || meta.pending).color }">
            {{ (meta[h.status] || meta.pending).label }}
          </span>
          <span class="hf-conf mono">{{ ((h.confidence || 0) * 100).toFixed(0) }}%</span>
        </div>
        <div class="hf-statement">{{ h.statement }}</div>
        <div class="hf-confbar">
          <div class="hf-confbar-fill" :style="{
            width: ((h.confidence || 0) * 100) + '%',
            background: (meta[h.status] || meta.pending).color,
          }" />
        </div>
        <div class="hf-refs" v-if="h.evidence_ids?.length">
          <button v-for="id in h.evidence_ids" :key="id" type="button" class="obs-badge"
                  @click="emit('open-evidence', id)">🔗 {{ id }}</button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.hf-row {
  display: flex; gap: 10px; padding: 10px 12px; border-radius: 10px; margin-bottom: 8px;
  border: 1px solid rgba(148, 163, 184, 0.14);
}
.hf-icon { font-size: 9px; font-weight: 700; letter-spacing: 0.5px; width: 68px; flex-shrink: 0; padding-top: 3px; font-family: var(--mono); }
.hf-main { flex: 1; min-width: 0; }
.hf-head { display: flex; align-items: center; justify-content: space-between; }
.hf-label { font-size: 11px; font-weight: 700; }
.hf-conf { font-size: 11px; color: #64748b; }
.hf-statement { font-size: 13px; color: #1e293b; line-height: 1.55; margin: 2px 0 6px; }
.hf-confbar { height: 4px; border-radius: 2px; background: #eef2f7; overflow: hidden; }
.hf-confbar-fill { height: 100%; border-radius: 2px; transition: width 0.4s ease; }
.hf-refs { margin-top: 6px; display: flex; gap: 6px; flex-wrap: wrap; }
.obs-badge {
  font-family: ui-monospace, monospace; font-size: 11px; color: var(--primary-dark);
  background: rgba(99, 102, 241, 0.08); border: 1px solid rgba(99, 102, 241, 0.2);
  padding: 1px 8px; border-radius: 8px; cursor: pointer; transition: all 0.15s ease;
}
.obs-badge:hover { background: rgba(99, 102, 241, 0.16); transform: translateY(-1px); }
</style>
