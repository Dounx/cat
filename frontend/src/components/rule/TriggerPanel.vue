<script setup lang="ts">
// The panel of a trigger node: where events come from, which of them start
// the rule, and how often at most.
import { computed } from 'vue'
import { useHub, summarize, timeAgo } from '../../hub'
import { useEditor } from '../../editor/context'
import { typesOf, type ENode } from '../../graph'
import { fieldLabel } from '../../labels'
import SchemaForm from '../SchemaForm.vue'
import PanelHead from './PanelHead.vue'

defineProps<{ node: ENode }>()
defineEmits<{ remove: [] }>()
const hub = useHub()
const { triggers, sourceInstances, listening, eventsOf, typeTitle, narrowed, anySource, anyType, triggerSchema } =
  useEditor()

// The events on offer for a trigger: what its sources say they emit, each with
// who emits it and the latest one actually received, so it reads as a real thing.
function offered(t: ENode) {
  const rows = new Map<string, { type: string; title: string; from: string[]; stray?: boolean }>()
  for (const inst of listening(t)) {
    for (const e of eventsOf(inst.plugin)) {
      const row = rows.get(e.type) || { type: e.type, title: e.title, from: [] }
      row.from.push(inst.name)
      rows.set(e.type, row)
    }
  }
  // still ticked, though no listened-to source emits it any more
  for (const type of t.pickedTypes) {
    if (!rows.has(type)) rows.set(type, { type, title: typeTitle(type) || type, from: [], stray: true })
  }
  const ids = new Set(listening(t).map((i) => i.id))
  return [...rows.values()].map((r) => ({
    ...r,
    last: hub.events.find((e) => e.type === r.type && ids.has(e.source)),
  }))
}
function widen(t: ENode, what: 'from' | 'type') {
  narrowed.delete(t.id + ':' + what)
  if (what === 'from') t.sources = []
  else Object.assign(t, { pickedTypes: [], otherTypes: '' })
}
const limitKeys = [
  { value: '', label: '所有事件合在一起算' },
  { value: 'actor.id', label: '每个发送者分别算' },
  { value: 'conversation.id', label: '每个会话分别算' },
]
function toggle(list: string[], value: string) {
  const i = list.indexOf(value)
  if (i >= 0) list.splice(i, 1)
  else list.push(value)
}
// The sources that are told what to watch in the trigger itself (a time, a
// feed, a page): offered up front, since choosing one is how that gets said.
const askable = computed(() => sourceInstances.value.filter((i) => hub.pluginOf(i.plugin)?.trigger_params))
function listenOnly(t: ENode, id: string) {
  narrowed.add(t.id + ':from')
  t.sources = [id]
}
</script>

<template>
  <PanelHead title="触发：什么事件会启动这条规则" :removable="triggers.length > 1" @remove="$emit('remove')" />
  <label class="label">从哪里来</label>
  <div class="seg mb-2">
    <button :class="{ on: anySource(node) }" @click="widen(node, 'from')">所有接入</button>
    <button :class="{ on: !anySource(node) }" @click="narrowed.add(node.id + ':from')">只听指定的</button>
  </div>
  <template v-if="anySource(node) && askable.length">
    <p class="mb-1.5 text-xs text-mute">定时、订阅和监控要先选中，才能填它什么时候触发、盯什么：</p>
    <div class="flex flex-wrap gap-1.5">
      <button v-for="s in askable" :key="s.id" class="btn text-xs" @click="listenOnly(node, s.id)">
        {{ s.name }}
      </button>
    </div>
  </template>
  <template v-if="!anySource(node)">
    <div v-if="sourceInstances.length" class="list">
      <label v-for="s in sourceInstances" :key="s.id" class="row link flex cursor-pointer items-center gap-3 px-3 py-2">
        <div class="min-w-0 flex-1">
          <div class="text-sm font-medium">{{ s.name }}</div>
          <div class="truncate text-xs text-mute">
            会发出：{{
              eventsOf(s.plugin)
                .map((e) => e.title)
                .join('、') || '（未声明）'
            }}
          </div>
        </div>
        <input type="checkbox" :checked="node.sources.includes(s.id)" @change="toggle(node.sources, s.id)" />
      </label>
    </div>
    <p v-if="!node.sources.length" class="mt-1 text-xs text-warn">还没有选。一个都不选时，按「所有接入」处理。</p>
    <SchemaForm
      v-if="triggerSchema(node)"
      class="mt-3"
      :model-value="node.params"
      :schema="triggerSchema(node)"
      @update:model-value="node.params = $event"
    />
  </template>
  <p v-if="!sourceInstances.length" class="text-xs text-mute">还没有能发出事件的接入，请先到「接入」添加渠道或监控。</p>

  <label class="label mt-5">发生什么事时触发</label>
  <div class="seg mb-2">
    <button :class="{ on: anyType(node) }" @click="widen(node, 'type')">任何事件</button>
    <button :class="{ on: !anyType(node) }" @click="narrowed.add(node.id + ':type')">只在指定的事发生时</button>
  </div>
  <template v-if="!anyType(node)">
    <div v-if="offered(node).length" class="list">
      <label
        v-for="e in offered(node)"
        :key="e.type"
        class="row link flex cursor-pointer items-center gap-3 px-3 py-2"
        :title="e.type"
      >
        <div class="min-w-0 flex-1">
          <div class="text-sm font-medium">{{ e.title }}</div>
          <div v-if="e.stray" class="truncate text-xs text-warn">上面选的接入不会发出这个事件</div>
          <template v-else>
            <div class="truncate text-xs text-mute">来自 {{ e.from.join('、') }}</div>
            <div class="truncate text-xs text-mute">
              <template v-if="e.last"
                >最近一次（{{ timeAgo(e.last.time) }}）：{{ summarize(e.last).slice(0, 80) || '（无内容）' }}</template
              >
              <template v-else>还没收到过</template>
            </div>
          </template>
        </div>
        <input
          type="checkbox"
          :checked="node.pickedTypes.includes(e.type)"
          @change="toggle(node.pickedTypes, e.type)"
        />
      </label>
    </div>
    <p v-if="!typesOf(node).length" class="mt-1 text-xs text-warn">还没有选。一个都不选时，按「任何事件」处理。</p>
    <details class="mt-3 text-xs text-mute" :open="!!node.otherTypes">
      <summary class="cursor-pointer">高级：按类型名匹配</summary>
      <input
        v-model="node.otherTypes"
        class="input mt-2 font-mono text-xs"
        placeholder="例如 message.*，多个用逗号分隔"
      />
      <p class="mt-1">用来匹配上面没列出的事件，支持 * 通配。</p>
    </details>
  </template>

  <label class="label mt-5">触发得太频繁时</label>
  <label class="flex cursor-pointer items-center gap-2 text-sm">
    <input
      type="checkbox"
      :checked="Number(node.limit) > 0"
      @change="node.limit = ($event.target as HTMLInputElement).checked ? 60 : 0"
    />
    限制频率：一段时间内只触发一次
  </label>
  <div v-if="Number(node.limit) > 0" class="mt-2 flex items-center gap-2 text-sm">
    <input v-model.number="node.limit" type="number" min="1" class="input w-24" />秒内只触发一次，
    <select v-model="node.limitKey" class="input min-w-0 flex-1">
      <option v-for="k in limitKeys" :key="k.value" :value="k.value">{{ k.label }}</option>
      <option v-if="!limitKeys.some((k) => k.value === node.limitKey)" :value="node.limitKey">
        按 {{ fieldLabel(node.limitKey) }} 分别算
      </option>
    </select>
  </div>
</template>
