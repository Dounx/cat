<script setup lang="ts">
// Renders a form from a JSON Schema object. With `templated`, every field
// accepts {{ expression }} placeholders, so numbers are edited as text.
import { computed, ref, watch } from 'vue'
import { History, List } from 'lucide-vue-next'
import { get } from '../api'
import type { Schema } from '../types'
import VarPicker, { type VarGroup } from './VarPicker.vue'
import TemplateInput from './TemplateInput.vue'
import MultiPick, { type Choice } from './MultiPick.vue'

// `variables` lists what a templated text field may refer to; when given,
// those fields show placeholders as named tags and offer a menu that inserts
// one at the cursor. `picks` gives, for the kinds of value a field may ask
// for with "x-pick", the ones seen in recent events. `optionsFrom` is where
// the instance behind the form lists the options of a field marked
// "x-pick": "option", e.g. "/instances/abc/options?action=chat".
export interface Picks {
  conversation?: VarGroup[]
  actor?: VarGroup[]
}
const props = defineProps<{
  schema?: Schema
  modelValue: Record<string, any>
  templated?: boolean
  variables?: VarGroup[]
  picks?: Picks
  optionsFrom?: string
}>()
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
  if (s.enum) return s['x-enum-notes'] ? 'choice' : 'enum'
  if (s['x-multi'] && !props.templated) return 'multi'
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

type Field = InstanceType<typeof TemplateInput>
const els: Record<string, Field> = {}
const bind = (key: string) => (el: unknown) => {
  if (el) els[key] = el as Field
  else delete els[key]
}
const pickable = (s: Schema) => !!props.variables && (kind(s) === 'text' || kind(s) === 'textarea')

// The name a placeholder goes by, when the variables on offer include it.
const names = computed(() => {
  const m = new Map<string, string>()
  for (const g of props.variables || []) for (const v of g.items) if (v.label) m.set(v.expr, v.label)
  return m
})
const labelOf = (expr: string) => names.value.get(expr.trim())

const enumTitle = (s: Schema, i: number) => s['x-enum-titles']?.[i] || s.enum![i]

// What the instance offers for the fields that ask it. A field has no entry
// until its answer is in; a failed answer is an empty list and its reason.
const offered = ref<Record<string, VarGroup[]>>({})
const refused = ref<Record<string, string>>({})
const asking = computed(() => fields.value.filter(([, s]) => s['x-pick'] === 'option').map(([key]) => key))
watch(
  () => [props.optionsFrom, asking.value.join(',')] as const,
  ([from], _old, stale) => {
    offered.value = {}
    refused.value = {}
    if (!from) return
    let current = true
    stale(() => (current = false))
    for (const key of asking.value) {
      get<{ value: string; title?: string; note?: string }[]>(`${from}&field=${encodeURIComponent(key)}`)
        .then((list) => [
          { title: '可以选的', items: list.map((o) => ({ expr: o.value, label: o.title || o.value, sample: o.note })) },
        ])
        .catch((e: Error) => {
          if (current) refused.value[key] = '没有取到可选项：' + e.message
          return [] as VarGroup[]
        })
        .then((groups) => {
          if (current) offered.value[key] = groups
        })
    }
  },
  { immediate: true },
)

// What a field that takes several values can choose from, as the instance lists it.
function choices(key: string): Choice[] {
  return (offered.value[key] || []).flatMap((g) =>
    g.items.map((o) => ({ value: o.expr, title: o.label, note: o.sample })),
  )
}
const waiting = (key: string, s: Schema) => s['x-pick'] === 'option' && !!props.optionsFrom && !(key in offered.value)

// Values to pick from for a field that asks for them.
const picksFor = (key: string, s: Schema) =>
  kind(s) === 'multi'
    ? undefined
    : s['x-pick'] === 'option'
      ? offered.value[key]
      : (s['x-pick'] && props.picks?.[s['x-pick']]) || undefined
function pick(key: string, s: Schema, value: string) {
  // senders add up to a list; a conversation replaces what was there
  const cur = show(key, s)
  const list = cur.split(/[,，\s]+/).filter(Boolean)
  const next = s['x-pick'] === 'actor' ? (list.includes(value) ? list : [...list, value]).join(',') : value
  if (els[key]) els[key].replace(next)
  else set(key, next)
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
      <div v-if="kind(s) !== 'boolean'" class="mb-1 flex items-center justify-between">
        <label class="label !mb-0">
          {{ s.title || key }}<span v-if="required.has(key)" class="text-bad"> *</span>
        </label>
        <span class="flex items-center">
          <VarPicker
            v-if="picksFor(key, s) && s['x-pick'] === 'option'"
            :groups="picksFor(key, s)!"
            :icon="List"
            text="从现有的里选"
            :empty="refused[key] || '没有可选的。'"
            @pick="pick(key, s, $event)"
          />
          <VarPicker
            v-else-if="picksFor(key, s)"
            :groups="picksFor(key, s)!"
            :icon="History"
            text="从最近的里选"
            empty="还没有收到过消息。先在那边发一条，这里就能选了。"
            @pick="pick(key, s, $event)"
          />
          <VarPicker v-if="pickable(s)" :groups="variables!" @pick="els[key]?.insert($event)" />
        </span>
      </div>
      <select
        v-if="kind(s) === 'enum'"
        class="input"
        :value="modelValue[key] ?? s.default ?? ''"
        @change="set(key, ($event.target as HTMLSelectElement).value)"
      >
        <option v-if="!required.has(key) && s.default === undefined" value="">（默认）</option>
        <option v-for="(o, i) in s.enum" :key="o" :value="o">{{ enumTitle(s, i) }}</option>
      </select>
      <div v-else-if="kind(s) === 'choice'" class="list">
        <label v-for="(o, i) in s.enum" :key="o" class="row link flex cursor-pointer items-start gap-3 px-3 py-2">
          <input
            type="radio"
            class="mt-1"
            :name="key"
            :checked="(modelValue[key] ?? s.default) === o"
            @change="set(key, o)"
          />
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium text-ink">{{ enumTitle(s, i) }}</div>
            <div v-if="s['x-enum-notes']?.[i]" class="text-xs text-mute">{{ s['x-enum-notes'][i] }}</div>
          </div>
        </label>
      </div>
      <MultiPick
        v-else-if="kind(s) === 'multi'"
        :model-value="show(key, s)"
        :options="choices(key)"
        :loading="waiting(key, s)"
        :empty="refused[key]"
        @update:model-value="set(key, $event)"
      />
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
      <TemplateInput
        v-else-if="pickable(s)"
        :ref="bind(key)"
        :model-value="show(key, s)"
        :multiline="kind(s) === 'textarea'"
        :placeholder="String(s.default ?? '')"
        :label-of="labelOf"
        @update:model-value="setText(key, s, $event)"
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
