<script setup lang="ts">
import { computed, ref } from 'vue'
import { ChevronRight, Plus, Workflow } from 'lucide-vue-next'
import { useHub } from '../hub'
import { put } from '../api'
import type { Rule } from '../types'
import PageHeader from '../components/PageHeader.vue'
import RuleEditor from '../components/RuleEditor.vue'

const hub = useHub()
const editing = ref<Rule | null>(null)

function onSaved() {
  editing.value = null
  hub.reloadRules()
}

function create() {
  editing.value = {
    id: '',
    name: '',
    enabled: true,
    sources: [],
    types: [],
    condition: '',
    steps: [],
    meta: { builder: { mode: 'builder', join: 'and', rows: [] } },
  }
}
// The most common setup, prefilled: a chat message goes to an agent and the
// answer comes back where the message was sent.
const chatSources = computed(() =>
  hub.instances.filter((i) => hub.pluginOf(i.plugin)?.actions?.some((a) => a.name === 'reply')),
)
const chatAgents = computed(() =>
  hub.instances.filter((i) => hub.pluginOf(i.plugin)?.actions?.some((a) => a.name === 'chat')),
)
function createChatRule() {
  const agent = chatAgents.value[0]
  editing.value = {
    id: '',
    name: `消息 → ${agent.name} → 回复`,
    enabled: true,
    sources: [],
    types: ['message.created'],
    condition: '',
    meta: {
      builder: { mode: 'builder', join: 'and', rows: [{ field: 'payload.mentioned', op: 'eq', value: 'true' }] },
    },
    steps: [
      { id: 'typing', instance: '$source', action: 'typing', params: {}, continue_on_error: true },
      { id: 'agent', instance: agent.id, action: 'chat', params: { prompt: '{{ payload.text }}' } },
      { id: 'reply', instance: '$source', action: 'reply', params: { text: '{{ steps.agent.text }}' } },
    ],
  }
}
async function toggle(r: Rule) {
  await hub.run(() => put(`/rules/${r.id}`, { ...r, enabled: !r.enabled, error: undefined }))
  hub.reloadRules()
}
function describe(r: Rule): string {
  const from = r.sources?.length ? r.sources.map(hub.instanceName).join('、') : '任意来源'
  // a graph may branch, so its actions are listed rather than chained
  const steps = r.graph ? r.graph.nodes.filter((n) => n.kind === 'action') : r.steps || []
  const to = steps
    .map((s) => `${s.instance === '$source' ? '原路' : hub.instanceName(s.instance)}·${s.action}`)
    .join(r.graph ? '、' : ' → ')
  return `${from} → ${to || '（无动作）'}`
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="规则" hint="事件满足条件时，依次执行动作">
      <button
        v-if="chatSources.length && chatAgents.length"
        class="btn"
        title="收到提到机器人的消息时，交给 Agent 并把回答发回原处"
        @click="createChatRule"
      >
        消息 → Agent → 回复
      </button>
      <button class="btn btn-primary" @click="create"><Plus :size="15" />新建规则</button>
    </PageHeader>
    <div class="gutter flex-1 overflow-y-auto pb-6">
      <div v-if="!hub.rules.length" class="card empty">
        <Workflow :size="28" :stroke-width="1.5" />
        <p class="text-ink">还没有规则</p>
        <p class="text-xs">新建一条，把事件和动作连起来。</p>
      </div>
      <TransitionGroup v-else tag="div" name="item" class="list">
        <div
          v-for="r in hub.rules"
          :key="r.id"
          class="row link flex cursor-pointer items-center gap-4 px-4 py-3"
          @click="editing = JSON.parse(JSON.stringify(r))"
        >
          <div class="min-w-0 flex-1 transition-opacity duration-200" :class="{ 'opacity-50': !r.enabled }">
            <div class="text-sm font-medium">{{ r.name }}</div>
            <div class="truncate text-xs text-mute">{{ describe(r) }}</div>
            <div v-if="r.condition" class="mt-1 truncate font-mono text-[11px] text-mute">{{ r.condition }}</div>
            <div v-if="r.error" class="mt-1 text-xs text-bad">{{ r.error }}</div>
          </div>
          <input
            type="checkbox"
            :checked="r.enabled"
            :title="r.enabled ? '已启用' : '已停用'"
            @click.stop
            @change="toggle(r)"
          />
          <ChevronRight :size="15" class="text-faint" />
        </div>
      </TransitionGroup>
    </div>
    <Transition name="drawer">
      <RuleEditor v-if="editing" :rule="editing" @close="editing = null" @saved="onSaved" />
    </Transition>
  </div>
</template>
