<script setup lang="ts">
// Renders a form from a JSON Schema object. With `templated`, every field
// accepts {{ expression }} placeholders, so numbers are edited as text.
import { computed } from 'vue'
import type { Schema } from '../types'

const props = defineProps<{ schema?: Schema; modelValue: Record<string, any>; templated?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: Record<string, any>] }>()

// JSON objects carry no order, so schemas list theirs in "x-order".
const fields = computed(() => {
  const entries = Object.entries(props.schema?.properties || {})
  const order = props.schema?.['x-order'] || []
  const rank = (k: string) => (order.indexOf(k) < 0 ? order.length : order.indexOf(k))
  return entries.sort((a, b) => rank(a[0]) - rank(b[0]))
})
const required = computed(() => new Set(props.schema?.required || []))

function set(key: string, value: unknown) {
  const next = { ...props.modelValue }
  if (value === '' || value === undefined) delete next[key]
  else next[key] = value
  emit('update:modelValue', next)
}

function kind(s: Schema): string {
  if (s.enum) return 'enum'
  if (s.type === 'boolean') return 'boolean'
  if ((s.type === 'number' || s.type === 'integer') && !props.templated) return 'number'
  if (s.type === 'object' || s.type === 'array') return 'json'
  if (s.format === 'textarea' || !s.type) return 'textarea'
  if (s.format === 'password') return 'password'
  return 'text'
}

function show(key: string, s: Schema): string {
  const v = props.modelValue[key]
  if (v === undefined || v === null) return ''
  return typeof v === 'string'
    ? v
    : kind(s) === 'json' || typeof v === 'object'
      ? JSON.stringify(v, null, 2)
      : String(v)
}

function setText(key: string, s: Schema, raw: string) {
  if (props.templated && (s.type === 'number' || s.type === 'integer') && raw.trim() !== '' && !isNaN(Number(raw))) {
    return set(key, Number(raw))
  }
  set(key, raw)
}

function setJSON(key: string, raw: string) {
  if (raw.trim() === '') return set(key, undefined)
  try {
    set(key, JSON.parse(raw))
  } catch {
    set(key, raw)
  }
}
</script>

<template>
  <div class="space-y-3">
    <div v-for="[key, s] in fields" :key="key">
      <label v-if="kind(s) !== 'boolean'" class="label">
        {{ s.title || key }}<span v-if="required.has(key)" class="text-bad"> *</span>
      </label>
      <select
        v-if="kind(s) === 'enum'"
        class="input"
        :value="modelValue[key] ?? s.default ?? ''"
        @change="set(key, ($event.target as HTMLSelectElement).value)"
      >
        <option v-if="!required.has(key) && s.default === undefined" value="">（默认）</option>
        <option v-for="o in s.enum" :key="o" :value="o">{{ o }}</option>
      </select>
      <label v-else-if="kind(s) === 'boolean'" class="flex cursor-pointer items-center gap-2 text-sm">
        <input
          type="checkbox"
          :checked="modelValue[key] ?? s.default ?? false"
          @change="set(key, ($event.target as HTMLInputElement).checked)"
        />
        {{ s.title || key }}
      </label>
      <input
        v-else-if="kind(s) === 'number'"
        type="number"
        class="input"
        :value="modelValue[key] ?? ''"
        :placeholder="String(s.default ?? '')"
        @input="
          set(
            key,
            ($event.target as HTMLInputElement).value === ''
              ? undefined
              : Number(($event.target as HTMLInputElement).value),
          )
        "
      />
      <textarea
        v-else-if="kind(s) === 'json'"
        class="input font-mono text-xs"
        rows="3"
        :value="show(key, s)"
        placeholder="{ }"
        @change="setJSON(key, ($event.target as HTMLTextAreaElement).value)"
      />
      <textarea
        v-else-if="kind(s) === 'textarea'"
        class="input font-mono text-xs"
        rows="3"
        :value="show(key, s)"
        @input="setText(key, s, ($event.target as HTMLTextAreaElement).value)"
      />
      <input
        v-else
        class="input"
        :type="kind(s) === 'password' ? 'password' : 'text'"
        :value="show(key, s)"
        :placeholder="String(s.default ?? '')"
        autocomplete="off"
        @input="setText(key, s, ($event.target as HTMLInputElement).value)"
      />
      <p v-if="s.description" class="mt-1 text-xs text-mute">{{ s.description }}</p>
    </div>
    <p v-if="!fields.length" class="text-xs text-mute">无需配置。</p>
  </div>
</template>
