<script setup lang="ts">
// A small menu of values to put in a field: what a templated field can refer
// to, or values seen in recent events. Picking one hands its expression to
// the field.
import { onBeforeUnmount, onMounted, ref, type Component } from 'vue'
import { Braces } from 'lucide-vue-next'

export interface VarGroup {
  title: string
  // `label` is what people read, `expr` what is inserted
  items: { expr: string; label?: string; sample?: string }[]
}

withDefaults(defineProps<{ groups: VarGroup[]; text?: string; icon?: Component; empty?: string }>(), {
  text: '插入变量',
  empty: '还没有可引用的值。',
})
const emit = defineEmits<{ pick: [expr: string] }>()

const open = ref(false)
const root = ref<HTMLElement>()

function outside(e: MouseEvent) {
  if (open.value && !root.value?.contains(e.target as Node)) open.value = false
}
// Escape closes the menu alone; caught early so the drawer behind stays open.
function onKey(e: KeyboardEvent) {
  if (!open.value || e.key !== 'Escape') return
  e.stopPropagation()
  open.value = false
}
onMounted(() => {
  window.addEventListener('mousedown', outside)
  window.addEventListener('keydown', onKey, true)
})
onBeforeUnmount(() => {
  window.removeEventListener('mousedown', outside)
  window.removeEventListener('keydown', onKey, true)
})

function pick(expr: string) {
  open.value = false
  emit('pick', expr)
}
</script>

<template>
  <span ref="root" class="relative inline-block">
    <!-- mousedown is held back so the field keeps its focus and cursor -->
    <button
      type="button"
      class="flex cursor-pointer items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] text-mute hover:bg-sunken hover:text-accent"
      :class="{ '!bg-accent-soft !text-accent': open }"
      @mousedown.prevent
      @click="open = !open"
    >
      <component :is="icon || Braces" :size="11" />{{ text }}
    </button>
    <div
      v-if="open"
      class="pop absolute right-0 z-10 mt-1 max-h-72 w-72 overflow-y-auto p-1.5"
      style="animation: item-in 0.16s var(--ease-glide)"
      @mousedown.prevent
    >
      <template v-for="g in groups" :key="g.title">
        <template v-if="g.items.length">
          <div class="px-2 pt-1.5 pb-1 text-[11px] font-medium text-mute">{{ g.title }}</div>
          <button
            v-for="v in g.items"
            :key="v.expr"
            type="button"
            class="flex w-full cursor-pointer items-baseline gap-2 rounded-md px-2 py-1 text-left hover:bg-sunken"
            :title="v.expr"
            @click="pick(v.expr)"
          >
            <span class="shrink-0 text-xs" :class="{ 'font-mono': !v.label }">{{ v.label || v.expr }}</span>
            <span class="truncate text-[11px] text-mute">{{ v.sample }}</span>
          </button>
        </template>
      </template>
      <p v-if="!groups.some((g) => g.items.length)" class="px-2 py-2 text-xs text-mute">{{ empty }}</p>
    </div>
  </span>
</template>
