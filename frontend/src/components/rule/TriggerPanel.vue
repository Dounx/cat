<script setup lang="ts">
// The panel of a trigger node: where events come from, which of them start
// the rule, and how often at most.
import { ref } from 'vue'
import { Zap } from 'lucide-vue-next'
import { useHub, summarize, timeAgo } from '../../hub'
import { useEditor } from '../../editor/context'
import { typesOf, type ENode } from '../../graph'
import SchemaForm from '../SchemaForm.vue'
import PanelHead from './PanelHead.vue'

const props = defineProps<{ node: ENode }>()
defineEmits<{ remove: [] }>()
const hub = useHub()
const {
  triggers,
  sourceInstances,
  listening,
  eventsOf,
  typeTitle,
  narrowed,
  anyType,
  triggerSchema,
  triggerTitle,
  targetIcon,
  pickSource,
} = useEditor()

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
function anyOfThem(t: ENode) {
  narrowed.delete(t.id + ':type')
  Object.assign(t, { pickedTypes: [], otherTypes: '' })
}
function toggle(list: string[], value: string) {
  const i = list.indexOf(value)
  if (i >= 0) list.splice(i, 1)
  else list.push(value)
}
// Where the events come from is picked from a list, as what an action uses
// is: everything, or one instance. Listening to several at once is ticked
// below it.
function resource() {
  pickSource('换一个来源', (instance) => (props.node.sources = instance ? [instance] : []))
}
const several = ref(props.node.sources.length > 1)
</script>

<template>
  <PanelHead title="触发：什么事件会启动这条规则" :removable="triggers.length > 1" @remove="$emit('remove')" />
  <label class="label">从哪里来</label>
  <button class="input flex w-full cursor-pointer items-center gap-2 text-left" title="换一个来源" @click="resource">
    <component
      :is="node.sources.length === 1 ? targetIcon(node.sources[0]) : Zap"
      :size="14"
      class="shrink-0 text-mute"
    />
    <span class="min-w-0 flex-1 truncate">{{ triggerTitle(node) }}</span>
    <span class="shrink-0 text-xs text-mute">更换</span>
  </button>
  <SchemaForm
    v-if="triggerSchema(node)"
    class="mt-3"
    :model-value="node.params"
    :schema="triggerSchema(node)"
    @update:model-value="node.params = $event"
  />
  <details
    v-if="sourceInstances.length > 1"
    class="mt-3 text-xs text-mute"
    :open="several"
    @toggle="several = ($event.target as HTMLDetailsElement).open"
  >
    <summary class="cursor-pointer">同时听多个接入</summary>
    <div class="list mt-2">
      <label v-for="s in sourceInstances" :key="s.id" class="row link flex cursor-pointer items-center gap-3 px-3 py-2">
        <div class="min-w-0 flex-1">
          <div class="text-sm font-medium text-ink">{{ s.name }}</div>
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
    <p class="mt-1">一个都不选时，听所有接入。</p>
  </details>
  <p v-if="!sourceInstances.length" class="mt-1 text-xs text-mute">
    还没有能发出事件的接入，点上面的「更换」接入一个。
  </p>

  <label class="label mt-5">发生什么事时触发</label>
  <div class="seg mb-2">
    <button :class="{ on: anyType(node) }" @click="anyOfThem(node)">任何事件</button>
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
</template>
