<script setup lang="ts">
import { ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ChevronRight, Plus } from 'lucide-vue-next'
import { useHub } from '../hub'
import { get, put } from '../api'
import type { HubEvent, Rule } from '../types'
import { ruleFromEvent } from '../templates'
import PageHeader from '../components/PageHeader.vue'
import RuleEditor from '../components/RuleEditor.vue'
import TemplatePicker from '../components/TemplatePicker.vue'

const hub = useHub()
const route = useRoute()
const router = useRouter()
const editing = ref<Rule | null>(null)
const choosing = ref(false)

function onSaved() {
  editing.value = null
  hub.reloadRules()
}
function start(rule: Rule) {
  choosing.value = false
  editing.value = rule
}

// Other pages send people here to make a rule: ?new after connecting
// something, ?event=<id> to start from an event that came in.
watch(
  () => route.query,
  async (q) => {
    if (typeof q.event === 'string') {
      const ev =
        hub.events.find((e) => e.id === q.event) || (await get<HubEvent>(`/events/${q.event}`).catch(() => null))
      if (ev) editing.value = ruleFromEvent(ev, hub.instanceName(ev.source))
    } else if ('new' in q) choosing.value = true
    else return
    router.replace({ path: '/rules' })
  },
  { immediate: true },
)

async function toggle(r: Rule) {
  await hub.run(() => put(`/rules/${r.id}`, { ...r, enabled: !r.enabled, error: undefined }))
  hub.reloadRules()
}
// "对话" for chat, as the plugin behind the step calls it.
function actionTitle(instance: string, action: string): string {
  const plugins = instance === '$source' ? hub.plugins : [hub.pluginOf(hub.instanceOf(instance)?.plugin || '')]
  return plugins.flatMap((p) => p?.actions || []).find((a) => a.name === action)?.title || action
}
function describe(r: Rule): string {
  const from = r.sources?.length ? r.sources.map(hub.instanceName).join('、') : '所有接入'
  // a graph may branch, so its actions are listed rather than chained
  const steps = r.graph ? r.graph.nodes.filter((n) => n.kind === 'action') : r.steps || []
  const to = steps
    .map((s) =>
      s.instance === '$source'
        ? actionTitle(s.instance, s.action)
        : `${hub.instanceName(s.instance)}·${actionTitle(s.instance, s.action)}`,
    )
    .join(r.graph ? '、' : ' → ')
  return `${from} → ${to || '（还没有动作）'}`
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="规则" hint="什么时候、满足什么条件、做什么">
      <button v-if="hub.rules.length" class="btn btn-primary" @click="choosing = true">
        <Plus :size="15" />新建规则
      </button>
    </PageHeader>
    <div class="gutter flex-1 overflow-y-auto pb-6">
      <!-- Nothing yet: start from what the rule is for. -->
      <div v-if="!hub.rules.length" class="mx-auto max-w-3xl pt-4">
        <h2 class="text-base font-semibold">你想让 Cat 做什么？</h2>
        <p class="mt-1 mb-4 text-sm text-mute">
          Cat 把各处来的消息和变化接到一起，按你定的规则处理。选一个开始，需要接入的东西会一步步带你接好。
        </p>
        <TemplatePicker inline @start="start" />
      </div>
      <TransitionGroup v-else tag="div" name="item" class="list">
        <div
          v-for="r in hub.rules"
          :key="r.id"
          class="row link flex cursor-pointer items-center gap-4 px-4 py-3"
          @click="editing = JSON.parse(JSON.stringify(r))"
        >
          <div class="min-w-0 flex-1 transition-opacity duration-200" :class="{ 'opacity-50': !r.enabled }">
            <div class="text-sm font-medium">{{ r.name || '未命名规则' }}</div>
            <div class="truncate text-xs text-mute">{{ describe(r) }}</div>
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
    <Transition name="modal">
      <TemplatePicker v-if="choosing" @start="start" @close="choosing = false" />
    </Transition>
    <Transition name="drawer">
      <RuleEditor v-if="editing" :rule="editing" @close="editing = null" @saved="onSaved" />
    </Transition>
  </div>
</template>
