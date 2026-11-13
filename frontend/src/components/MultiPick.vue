<script setup lang="ts">
// Several values out of a list that may be long: what is chosen shows as
// tags, and typing narrows the list. A value the list doesn't have can be
// typed in all the same. The value is the chosen ones, one per line.
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Check, Plus, X } from 'lucide-vue-next'

export interface Choice {
  value: string
  title?: string
  note?: string
}
const props = defineProps<{ modelValue: string; options?: Choice[]; loading?: boolean; empty?: string }>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

// no more rows than can be looked through; typing brings the rest
const MAX_ROWS = 60

const chosen = computed(() =>
  props.modelValue
    .split(/[\n,]+/)
    .map((s) => s.trim())
    .filter(Boolean),
)
const query = ref('')
const open = ref(false)
const at = ref(0) // the row the keyboard is on
const root = ref<HTMLElement>()
const box = ref<HTMLInputElement>()

const matches = computed(() => {
  const q = query.value.trim().toLowerCase()
  const all = props.options || []
  return q ? all.filter((o) => `${o.value} ${o.title || ''}`.toLowerCase().includes(q)) : all
})
const rows = computed(() => matches.value.slice(0, MAX_ROWS))
// what was typed, when it is neither on the list nor chosen: it can be added as it is
const typed = computed(() => {
  const q = query.value.trim()
  const known = (props.options || []).some((o) => o.value.toLowerCase() === q.toLowerCase())
  return q && !known && !chosen.value.includes(q) ? q : ''
})
const count = computed(() => rows.value.length + (typed.value ? 1 : 0))
watch(query, () => (at.value = 0))

function toggle(value: string) {
  const has = chosen.value.includes(value)
  emit('update:modelValue', (has ? chosen.value.filter((v) => v !== value) : [...chosen.value, value]).join('\n'))
  query.value = ''
  box.value?.focus()
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault()
    open.value = true
    if (count.value) at.value = (at.value + (e.key === 'ArrowDown' ? 1 : count.value - 1)) % count.value
  } else if (e.key === 'Enter') {
    e.preventDefault() // not the form's to submit
    const row = rows.value[at.value]
    if (open.value && row) toggle(row.value)
    else if (typed.value) toggle(typed.value)
  } else if (e.key === 'Backspace' && !query.value && chosen.value.length) {
    toggle(chosen.value[chosen.value.length - 1])
  }
}

function outside(e: MouseEvent) {
  if (open.value && !root.value?.contains(e.target as Node)) open.value = false
}
// Escape closes the list alone; caught early so the dialog behind stays open.
function onEscape(e: KeyboardEvent) {
  if (!open.value || e.key !== 'Escape') return
  e.stopPropagation()
  open.value = false
}
onMounted(() => {
  window.addEventListener('mousedown', outside)
  window.addEventListener('keydown', onEscape, true)
})
onBeforeUnmount(() => {
  window.removeEventListener('mousedown', outside)
  window.removeEventListener('keydown', onEscape, true)
})
</script>

<template>
  <div ref="root" class="relative">
    <div class="input flex cursor-text flex-wrap items-center gap-1 !py-1.5" @click="box?.focus()">
      <span v-for="v in chosen" :key="v" class="chip gap-1 !text-ink">
        {{ v }}
        <button
          type="button"
          class="cursor-pointer text-mute hover:text-bad"
          :title="`去掉 ${v}`"
          @click.stop="toggle(v)"
        >
          <X :size="11" />
        </button>
      </span>
      <input
        ref="box"
        v-model="query"
        class="min-w-24 flex-1 bg-transparent py-0.5 text-sm outline-none"
        :placeholder="chosen.length ? '' : '搜索，或直接输入'"
        autocomplete="off"
        @focus="open = true"
        @input="open = true"
        @keydown="onKey"
      />
    </div>
    <div v-if="open" class="pop absolute right-0 left-0 z-10 mt-1 max-h-64 overflow-y-auto p-1.5" @mousedown.prevent>
      <button
        v-for="(o, i) in rows"
        :key="o.value"
        type="button"
        class="flex w-full cursor-pointer items-baseline gap-2 rounded-md px-2 py-1 text-left hover:bg-sunken"
        :class="{ 'bg-sunken': i === at }"
        @click="toggle(o.value)"
      >
        <Check :size="12" class="shrink-0 self-center text-accent" :class="{ invisible: !chosen.includes(o.value) }" />
        <span class="shrink-0 text-xs">{{ o.title || o.value }}</span>
        <span class="truncate text-[11px] text-mute">{{ o.note }}</span>
      </button>
      <button
        v-if="typed"
        type="button"
        class="flex w-full cursor-pointer items-center gap-2 rounded-md px-2 py-1 text-left text-xs hover:bg-sunken"
        :class="{ 'bg-sunken': at === rows.length }"
        @click="toggle(typed)"
      >
        <Plus :size="12" class="shrink-0 text-accent" />添加「{{ typed }}」
      </button>
      <p v-if="matches.length > rows.length" class="px-2 py-1 text-[11px] text-mute">
        还有 {{ matches.length - rows.length }} 个，输入几个字缩小范围。
      </p>
      <p v-if="!count" class="px-2 py-2 text-xs text-mute">
        {{ loading ? '正在取可选项…' : empty || '没有可选的，直接输入后按回车添加。' }}
      </p>
    </div>
  </div>
</template>
