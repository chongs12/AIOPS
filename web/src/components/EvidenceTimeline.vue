<script setup>
// 证据时间轴：事件节点 + 可点击证据徽标（点击抛出 open-evidence 事件）。
import { computed } from 'vue'

const props = defineProps({
  items: { type: Array, default: () => [] }, // [{time?, event, source}]
})
const emit = defineEmits(['open-evidence'])

const groups = computed(() =>
  props.items.map(it => ({
    ...it,
    obsRefs: (it.source || '').match(/obs-[a-f0-9]+/g) || [],
  })))
</script>

<template>
  <div class="tl" v-if="items.length">
    <div v-for="(g, i) in groups" :key="i" class="tl-item">
      <div class="tl-dot" :class="{ last: i === groups.length - 1 }" />
      <div class="tl-body">
        <div class="tl-text">{{ g.event }}</div>
        <div class="tl-refs" v-if="g.obsRefs.length || g.source">
          <button v-for="ref in g.obsRefs" :key="ref" type="button" class="obs-badge"
                  @click="emit('open-evidence', ref)">🔗 {{ ref }}</button>
          <span v-if="!g.obsRefs.length && g.source" class="obs-badge" style="cursor: default">{{ g.source }}</span>
        </div>
      </div>
    </div>
  </div>
  <div v-else class="tl-empty muted">时间线暂无事件</div>
</template>

<style scoped>
.tl { position: relative; padding-left: 18px; }
.tl::before { content: ''; position: absolute; left: 5px; top: 6px; bottom: 6px; width: 2px; background: #e2e8f0; border-radius: 2px; }
.tl-item { position: relative; padding: 4px 0 10px; }
.tl-dot {
  position: absolute; left: -18px; top: 8px; width: 12px; height: 12px; border-radius: 50%;
  background: #fff; border: 3px solid var(--primary-light); box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.12);
}
.tl-dot.last { border-color: var(--success); box-shadow: 0 0 0 3px rgba(16, 185, 129, 0.15); }
.tl-text { font-size: 13px; color: #334155; line-height: 1.55; }
.tl-refs { margin-top: 4px; display: flex; gap: 6px; flex-wrap: wrap; }
.obs-badge {
  font-family: ui-monospace, monospace; font-size: 11px; color: var(--primary-dark);
  background: rgba(99, 102, 241, 0.08); border: 1px solid rgba(99, 102, 241, 0.2);
  padding: 1px 8px; border-radius: 8px; cursor: pointer; transition: all 0.15s ease;
}
.obs-badge:hover { background: rgba(99, 102, 241, 0.16); transform: translateY(-1px); }
.tl-empty { font-size: 12px; padding: 6px 0; }
</style>
