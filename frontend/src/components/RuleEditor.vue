<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { ArrowDown, ArrowUp, Plus, Trash2 } from 'lucide-vue-next'
import { useHub, clock, summarize } from '../hub'
import { del, post, put } from '../api'
import type { ActionSpec, Rule, Step } from '../types'
import { commonFields, compile, operators, type Builder } from '../condition'
import Drawer from './Drawer.vue'
import SchemaForm from './SchemaForm.vue'

const props = defineProps<{ rule: Rule }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const hub = useHub()

const rule = reactive<Rule>(JSON.parse(JSON.stringify(props.rule)))
rule.sources ||= []
rule.steps ||= []
const builder = reactive<Builder>(rule.meta?.builder || { mode: rule.condition ? 'expr' : 'builder', join: 'and', rows: [] })
const typesText = ref((rule.types || []).join(', '))
const testEventId = ref('')
const testResult = ref<null | { matched: boolean; error?: string }>(null)

const sourceInstances = computed(() => hub.instances.filter((i) => hub.pluginOf(i.plugin)?.capabilities.includes('source')))
const actionInstances = computed(() => hub.instances.filter((i) => hub.pluginOf(i.plugin)?.actions?.length))
const knownTypes = computed(() => [...new Set(hub.plugins.flatMap((p) => p.events?.map((e) => e.type) || []))])
const condition = computed(() => (builder.mode === 'builder' ? compile(builder) : rule.condition || ''))
const scopedEvents = computed(() => hub.events.filter((e) => !rule.sources!.length || rule.sources!.includes(e.source)).slice(0, 30))
const fieldHints = computed(() => {
  const ev = hub.events.find((e) => e.id === testEventId.value) || scopedEvents.value[0]
  const extra = ev ? Object.keys(ev.payload || {}).map((k) => `payload.${k}`) : []
  return [...new Set([...commonFields, ...extra])]
})

watch([condition, testEventId], () => { testResult.value = null })

function toggleSource(id: string) {
  const i = rule.sources!.indexOf(id)
  if (i >= 0) rule.sources!.splice(i, 1)
  else rule.sources!.push(id)
}

// Actions offered for a step. "$source" replies through whichever instance
// emitted the event, so offer what the rule's possible sources can do.
function actionsFor(step: Step): ActionSpec[] {
  if (step.instance !== '$source') return hub.pluginOf(hub.instanceOf(step.instance)?.plugin || '')?.actions || []
  const pool = rule.sources!.length ? sourceInstances.value.filter((i) => rule.sources!.includes(i.id)) : sourceInstances.value
  const seen = new Map<string, ActionSpec>()
  for (const i of pool) for (const a of hub.pluginOf(i.plugin)?.actions || []) seen.set(a.name, a)
  return [...seen.values()]
}
const specOf = (step: Step) => actionsFor(step).find((a) => a.name === step.action)

// Start a step's parameters from the defaults its schema declares.
function resetParams(step: Step) {
  const params: Record<string, unknown> = {}
  for (const [key, s] of Object.entries(specOf(step)?.params?.properties || {})) {
    if (s.default !== undefined) params[key] = s.default
  }
  step.params = params
}
function addStep() {
  const first = actionInstances.value[0]
  const step: Step = { id: `s${rule.steps.length + 1}`, instance: first?.id || '', action: hub.pluginOf(first?.plugin || '')?.actions?.[0]?.name || '', params: {} }
  resetParams(step)
  rule.steps.push(step)
}
function onTarget(step: Step) {
  const list = actionsFor(step)
  if (!list.some((a) => a.name === step.action)) step.action = list[0]?.name || ''
  resetParams(step)
}
function move(i: number, d: number) {
  const [s] = rule.steps.splice(i, 1)
  rule.steps.splice(i + d, 0, s)
}

async function test() {
  const id = testEventId.value || scopedEvents.value[0]?.id
  if (!id) return hub.notify('还没有可用于测试的事件', true)
  testEventId.value = id
  testResult.value = (await hub.run(() => post('/rules/test', { condition: condition.value, event_id: id }))) || null
}

function payload(): Rule {
  return {
    ...JSON.parse(JSON.stringify(rule)),
    types: typesText.value.split(/[,，\s]+/).filter(Boolean),
    condition: condition.value,
    throttle_seconds: Number(rule.throttle_seconds) || 0,
    meta: { ...rule.meta, builder: JSON.parse(JSON.stringify(builder)) },
    error: undefined,
  }
}
async function save() {
  const body = payload()
  const ok = await hub.run(() => (rule.id ? put(`/rules/${rule.id}`, body) : post('/rules', body)), '规则已保存')
  if (ok) emit('saved')
}
async function remove() {
  if (!confirm(`删除规则「${rule.name}」？`)) return
  await hub.run(() => del(`/rules/${rule.id}`), '已删除')
  emit('saved')
}
</script>

<template>
  <Drawer :title="rule.id ? '编辑规则' : '新建规则'" wide @close="emit('close')">
    <div class="space-y-6">
      <section class="flex items-end gap-3">
        <div class="flex-1"><label class="label">名称</label><input v-model="rule.name" class="input" placeholder="例如：部署通知触发 Agent" /></div>
        <label class="flex items-center gap-1.5 pb-2 text-sm"><input v-model="rule.enabled" type="checkbox" />启用</label>
      </section>

      <section>
        <h3 class="mb-2 text-sm font-semibold">① 当这些来源收到事件</h3>
        <div class="flex flex-wrap gap-2">
          <button v-for="s in sourceInstances" :key="s.id" class="btn" :class="{ '!border-accent !bg-accent-soft !text-accent': rule.sources!.includes(s.id) }" @click="toggleSource(s.id)">{{ s.name }}</button>
          <span v-if="!sourceInstances.length" class="text-xs text-mute">还没有能收到事件的渠道，请先到「接入」添加。</span>
        </div>
        <p class="mt-1 text-xs text-mute">不选表示任意来源。</p>
        <label class="label mt-3">事件类型（可选，逗号分隔，支持 message.* 通配）</label>
        <input v-model="typesText" class="input" list="known-types" placeholder="任意类型" />
        <datalist id="known-types"><option v-for="t in knownTypes" :key="t" :value="t" /></datalist>
      </section>

      <section>
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-sm font-semibold">② 并且满足条件</h3>
          <div class="flex overflow-hidden rounded-md border border-line text-xs">
            <button class="px-2.5 py-1" :class="builder.mode === 'builder' ? 'bg-accent-soft text-accent' : 'bg-surface text-mute'" @click="builder.mode = 'builder'">可视化</button>
            <button class="px-2.5 py-1" :class="builder.mode === 'expr' ? 'bg-accent-soft text-accent' : 'bg-surface text-mute'" @click="rule.condition = condition; builder.mode = 'expr'">表达式</button>
          </div>
        </div>

        <template v-if="builder.mode === 'builder'">
          <div v-for="(row, i) in builder.rows" :key="i" class="mb-2 flex items-center gap-2">
            <span class="w-8 shrink-0 text-center text-xs text-mute">{{ i === 0 ? '当' : builder.join === 'and' ? '且' : '或' }}</span>
            <input v-model="row.field" class="input flex-1 font-mono text-xs" list="field-hints" placeholder="payload.text" />
            <select v-model="row.op" class="input w-28"><option v-for="o in operators" :key="o.id" :value="o.id">{{ o.label }}</option></select>
            <input v-if="!operators.find((o) => o.id === row.op)?.noValue" v-model="row.value" class="input flex-1" :placeholder="row.op === 'in' ? '用逗号分隔' : '值'" />
            <button class="btn btn-ghost px-2" aria-label="删除条件" @click="builder.rows.splice(i, 1)"><Trash2 :size="14" /></button>
          </div>
          <datalist id="field-hints"><option v-for="f in fieldHints" :key="f" :value="f" /></datalist>
          <div class="flex items-center gap-3">
            <button class="btn" @click="builder.rows.push({ field: 'payload.text', op: 'contains', value: '' })"><Plus :size="14" />添加条件</button>
            <select v-if="builder.rows.length > 1" v-model="builder.join" class="input w-36"><option value="and">全部满足</option><option value="or">满足任一</option></select>
            <span v-if="!builder.rows.length" class="text-xs text-mute">没有条件表示所有事件都命中。</span>
          </div>
          <p v-if="condition" class="mt-2 font-mono text-[11px] break-all text-mute">{{ condition }}</p>
        </template>
        <template v-else>
          <textarea v-model="rule.condition" class="input font-mono text-xs" rows="3" placeholder='payload.text contains "deploy" and actor.id in ["123", "456"]' />
          <p class="mt-1 text-xs text-mute">expr 表达式。可用变量：payload、actor、conversation、type、source、raw。留空表示全部命中。</p>
        </template>

        <div class="mt-3 flex items-center gap-2">
          <select v-model="testEventId" class="input flex-1 text-xs">
            <option value="">用最近一个事件测试</option>
            <option v-for="e in scopedEvents" :key="e.id" :value="e.id">{{ clock(e.time) }} · {{ e.type }} · {{ summarize(e).slice(0, 60) }}</option>
          </select>
          <button class="btn" @click="test">测试条件</button>
          <span v-if="testResult" class="text-sm font-medium" :class="testResult.error ? 'text-bad' : testResult.matched ? 'text-ok' : 'text-mute'">
            {{ testResult.error ? '出错' : testResult.matched ? '命中' : '不命中' }}
          </span>
        </div>
        <p v-if="testResult?.error" class="mt-1 text-xs text-bad">{{ testResult.error }}</p>
      </section>

      <section>
        <h3 class="mb-2 text-sm font-semibold">③ 依次执行</h3>
        <div v-for="(step, i) in rule.steps" :key="i" class="card mb-3 p-4">
          <div class="mb-3 flex items-center gap-2">
            <span class="text-sm font-medium text-mute">{{ i + 1 }}</span>
            <select v-model="step.instance" class="input flex-1" @change="onTarget(step)">
              <option value="$source">事件来源（原路回复）</option>
              <option v-for="a in actionInstances" :key="a.id" :value="a.id">{{ a.name }}</option>
            </select>
            <select v-model="step.action" class="input flex-1" @change="resetParams(step)">
              <option v-for="a in actionsFor(step)" :key="a.name" :value="a.name">{{ a.title }}</option>
            </select>
            <button class="btn btn-ghost px-1.5" :disabled="i === 0" aria-label="上移" @click="move(i, -1)"><ArrowUp :size="14" /></button>
            <button class="btn btn-ghost px-1.5" :disabled="i === rule.steps.length - 1" aria-label="下移" @click="move(i, 1)"><ArrowDown :size="14" /></button>
            <button class="btn btn-ghost btn-danger px-1.5" aria-label="删除步骤" @click="rule.steps.splice(i, 1)"><Trash2 :size="14" /></button>
          </div>
          <p v-if="specOf(step)?.description" class="mb-3 text-xs text-mute">{{ specOf(step)!.description }}</p>
          <SchemaForm :model-value="step.params || {}" :schema="specOf(step)?.params" templated @update:model-value="step.params = $event" />
          <details class="mt-3 text-xs text-mute">
            <summary class="cursor-pointer">高级</summary>
            <div class="mt-2 flex items-center gap-4">
              <label class="flex items-center gap-2">步骤标识<input v-model="step.id" class="input w-28 font-mono text-xs" /></label>
              <label class="flex items-center gap-1.5"><input v-model="step.continue_on_error" type="checkbox" />失败后继续执行后续步骤</label>
            </div>
          </details>
        </div>
        <button class="btn" @click="addStep"><Plus :size="14" />添加动作</button>
        <p class="mt-2 text-xs text-mute">
          参数里可以用 <code class="chip" v-text="'{{ payload.text }}'" /> 引用事件，用 <code class="chip" v-text="'{{ steps.s1.text }}'" /> 引用前面步骤的输出。
        </p>
      </section>

      <section>
        <h3 class="mb-2 text-sm font-semibold">节流（可选）</h3>
        <div class="flex items-center gap-2 text-sm">
          同一个<input v-model="rule.throttle_key" class="input w-48 font-mono text-xs" placeholder="规则（或如 actor.id）" />
          在<input v-model="rule.throttle_seconds" type="number" min="0" class="input w-20" />秒内只触发一次
        </div>
      </section>
    </div>

    <template #footer>
      <button v-if="rule.id" class="btn btn-danger mr-auto" @click="remove">删除</button>
      <button class="btn" @click="emit('close')">取消</button>
      <button class="btn btn-primary" @click="save">保存</button>
    </template>
  </Drawer>
</template>
