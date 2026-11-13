<script setup lang="ts">
// The top of a node's panel: what the node is, and the ways out of it.
import { Trash2, X } from 'lucide-vue-next'
import { useEditor } from '../../editor/context'

withDefaults(defineProps<{ title: string; removable?: boolean }>(), { removable: true })
defineEmits<{ remove: [] }>()
const { sel } = useEditor()
</script>

<template>
  <div class="mb-3 flex items-center gap-1">
    <h3 class="mr-auto truncate text-sm font-semibold">{{ title }}</h3>
    <slot />
    <button
      v-if="removable"
      class="btn btn-ghost btn-danger px-1.5"
      title="删除节点（Del）"
      aria-label="删除节点"
      @click="$emit('remove')"
    >
      <Trash2 :size="14" />
    </button>
    <button class="btn btn-ghost px-1.5" title="收起（Esc）" aria-label="收起" @click="sel = null">
      <X :size="14" />
    </button>
  </div>
</template>
