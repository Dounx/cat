<script setup lang="ts">
// A menu opened at the pointer by a right click. It closes on a choice, a
// click elsewhere, Escape, or when the window scrolls or loses focus.
import { nextTick, onBeforeUnmount, onMounted, ref, type Component } from 'vue'

export interface MenuItem {
  label: string
  icon?: Component
  hint?: string // a shortcut, shown at the right
  danger?: boolean
  disabled?: boolean
  divided?: boolean // a hairline above this item
  run: () => void
}

const props = defineProps<{ x: number; y: number; items: MenuItem[] }>()
const emit = defineEmits<{ close: [] }>()

const root = ref<HTMLElement>()
const pos = ref({ left: props.x, top: props.y })

function outside(e: Event) {
  if (!root.value?.contains(e.target as Node)) emit('close')
}
// Escape closes the menu alone; caught early so the drawer behind stays open.
function onKey(e: KeyboardEvent) {
  if (e.key !== 'Escape') return
  e.stopPropagation()
  emit('close')
}
const close = () => emit('close')

onMounted(() => {
  // keep the whole menu on screen
  nextTick(() => {
    const el = root.value
    if (!el) return
    pos.value = {
      left: Math.max(4, Math.min(props.x, window.innerWidth - el.offsetWidth - 4)),
      top: Math.max(4, Math.min(props.y, window.innerHeight - el.offsetHeight - 4)),
    }
    el.querySelector<HTMLElement>('button:not(:disabled)')?.focus({ preventScroll: true })
  })
  window.addEventListener('mousedown', outside, true)
  window.addEventListener('keydown', onKey, true)
  window.addEventListener('wheel', close, true)
  window.addEventListener('blur', close)
  window.addEventListener('resize', close)
})
onBeforeUnmount(() => {
  window.removeEventListener('mousedown', outside, true)
  window.removeEventListener('keydown', onKey, true)
  window.removeEventListener('wheel', close, true)
  window.removeEventListener('blur', close)
  window.removeEventListener('resize', close)
})

function choose(item: MenuItem) {
  emit('close')
  item.run()
}
// Arrow keys walk the items.
function step(e: KeyboardEvent, d: number) {
  const list = [...(root.value?.querySelectorAll<HTMLElement>('button:not(:disabled)') || [])]
  const i = list.indexOf(e.target as HTMLElement)
  list[(i + d + list.length) % list.length]?.focus()
}
</script>

<template>
  <Teleport to="body">
    <div
      ref="root"
      role="menu"
      class="pop fixed z-50 min-w-44 p-1"
      :style="{ left: pos.left + 'px', top: pos.top + 'px', animation: 'item-in 0.14s var(--ease-glide)' }"
      @contextmenu.prevent
      @keydown.down.prevent="step($event, 1)"
      @keydown.up.prevent="step($event, -1)"
    >
      <template v-for="item in items" :key="item.label">
        <div v-if="item.divided" class="mx-1 my-1 border-t border-line" />
        <button
          type="button"
          role="menuitem"
          class="item"
          :class="{ danger: item.danger }"
          :disabled="item.disabled"
          @click="choose(item)"
        >
          <component :is="item.icon" v-if="item.icon" :size="14" class="shrink-0" />
          {{ item.label }}
          <span v-if="item.hint" class="ml-auto pl-6 text-[11px] text-mute">{{ item.hint }}</span>
        </button>
      </template>
    </div>
  </Teleport>
</template>

<style scoped>
.item {
  display: flex;
  width: 100%;
  align-items: center;
  gap: 8px;
  padding: 5px 10px;
  border-radius: 7px;
  font-size: 13px;
  text-align: left;
  white-space: nowrap;
  cursor: pointer;
  outline: none;
}
.item:hover,
.item:focus-visible {
  background: var(--hover);
}
.item:disabled {
  opacity: 0.4;
  pointer-events: none;
}
.item.danger {
  color: var(--color-bad);
}
.item.danger:hover,
.item.danger:focus-visible {
  background: color-mix(in srgb, var(--color-bad) 12%, transparent);
}
</style>
