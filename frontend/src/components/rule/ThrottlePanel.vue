<script setup lang="ts">
// The panel of a throttle node.
import { useEditor } from '../../editor/context'
import type { ENode } from '../../graph'
import PanelHead from './PanelHead.vue'

defineProps<{ node: ENode }>()
defineEmits<{ remove: [] }>()
const { fieldHints } = useEditor()
</script>

<template>
  <PanelHead title="节流" @remove="$emit('remove')" />
  <p class="mb-4 text-xs text-mute">
    事件走到这里时，一段时间内只放行第一个，其余的到此为止，后面的节点不会执行。只影响经过它的这条线。
  </p>
  <label class="label">多长时间内只放行一次</label>
  <div class="flex items-center gap-2 text-sm">
    <input v-model="node.seconds" type="number" min="0" class="input w-28" />秒
  </div>
  <label class="label mt-4">分别计算（可选）</label>
  <input
    v-model="node.key"
    class="input font-mono text-xs"
    list="field-hints-throttle"
    placeholder="留空：所有事件共用一个计时"
  />
  <datalist id="field-hints-throttle"><option v-for="f in fieldHints" :key="f" :value="f" /></datalist>
  <p class="mt-1 text-xs text-mute">
    填一个字段，就按它的值各自计时。例如填 <code class="chip">actor.id</code> 表示每个人各自
    {{ Number(node.seconds) || 0 }} 秒内只放行一次。
  </p>
</template>
