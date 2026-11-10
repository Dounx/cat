<script setup lang="ts">
import { onBeforeUnmount, onMounted } from 'vue'
import { X } from 'lucide-vue-next'

// `full` gives the sheet the whole window and leaves the body to its content:
// no padding and no scrolling of its own. The `header` slot replaces the title
// bar, and the `top` slot sits above the body.
defineProps<{ title: string; wide?: boolean; full?: boolean }>()
const emit = defineEmits<{ close: [] }>()

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') emit('close')
}
onMounted(() => window.addEventListener('keydown', onKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>

<!-- Shown inside <Transition name="drawer"> by its caller, so it slides out as
     well as in. -->
<template>
  <div class="scrim fixed inset-0 z-40 flex justify-end p-2" @click.self="emit('close')">
    <section
      class="pop flex h-full w-full flex-col overflow-hidden"
      :class="full ? 'max-w-none' : wide ? 'max-w-3xl' : 'max-w-xl'"
    >
      <slot name="header">
        <header class="flex items-center justify-between gap-3 border-b border-line px-5 py-3">
          <h2 class="truncate text-[15px] font-semibold">{{ title }}</h2>
          <div class="flex items-center gap-2">
            <slot name="actions" />
            <button class="btn btn-ghost px-1.5" aria-label="关闭" @click="emit('close')"><X :size="16" /></button>
          </div>
        </header>
      </slot>
      <slot name="top" />
      <div :class="full ? 'relative flex min-h-0 flex-1' : 'flex-1 overflow-y-auto p-5'"><slot /></div>
      <footer v-if="$slots.footer" class="flex justify-end gap-2 border-t border-line px-5 py-3">
        <slot name="footer" />
      </footer>
    </section>
  </div>
</template>
