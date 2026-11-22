<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ChevronRight, FileDown, Plus } from 'lucide-vue-next'
import { useHub } from '../hub'
import { get, put } from '../api'
import type { HubEvent, Rule, RuleStats } from '../types'
import { formatTokens, relativeTime, summarizeRules, tokenBreakdown } from '../metrics'
import { ruleFromEvent } from '../templates'
import PageHeader from '../components/PageHeader.vue'
import RuleEditor from '../components/RuleEditor.vue'
import TemplatePicker from '../components/TemplatePicker.vue'
import RuleImport from '../components/RuleImport.vue'

const hub = useHub()
const route = useRoute()
const router = useRouter()
const editing = ref<Rule | null>(null)
const choosing = ref(false)
const importing = ref(false)

const summary = computed(() => summarizeRules(hub.rules))
const hasSummary = computed(() => summary.value.runs24h > 0 || summary.value.running > 0)

onMounted(() => {
  if (hub.rules.length) void hub.reloadRuleStats()
})

function statsDot(st: RuleStats): string {
  if (st.running > 0) return 'warn'
  if (st.failed_24h > 0) return 'bad'
  if (st.runs_24h > 0) return 'ok'
  return ''
}

function onSaved() {
  editing.value = null
  hub.reloadRules()
}
function start(rule: Rule) {
  choosing.value = importing.value = false
  editing.value = rule
}

// Other pages send people here to make a rule: ?new after connecting
// something, ?event=<id> to start from an event that came in. ?rule=<id>
// opens one there is.
watch(
  () => route.query,
  async (q) => {
    if (typeof q.event === 'string') {
      const ev =
        hub.events.find((e) => e.id === q.event) || (await get<HubEvent>(`/events/${q.event}`).catch(() => null))
      if (ev) editing.value = ruleFromEvent(ev, hub.instanceName(ev.source))
    } else if (typeof q.rule === 'string') {
      const rule = hub.rules.find((r) => r.id === q.rule)
      if (rule) editing.value = JSON.parse(JSON.stringify(rule))
    } else if ('new' in q) choosing.value = true
    else return
    router.replace({ path: '/rules' })
  },
  { immediate: true },
)

async function toggle(r: Rule) {
  await hub.run(() => put(`/rules/${r.id}`, { ...r, enabled: !r.enabled, error: undefined, stats: undefined }))
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
      <button class="btn" title="导入别人导出的规则" @click="importing = true"><FileDown :size="15" />导入</button>
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
      <template v-else>
        <div
          v-if="hasSummary"
          class="mb-2.5 flex flex-wrap items-center gap-x-2 gap-y-1 px-1 text-xs text-mute tabular-nums"
        >
          <span>近 24 小时</span>
          <span class="text-faint">·</span>
          <span>共 {{ summary.runs24h }} 次运行</span>
          <template v-if="summary.failed24h > 0">
            <span class="text-faint">·</span>
            <span class="inline-flex items-center gap-1.5 font-medium text-bad">
              <span class="dot bad !h-1.5 !w-1.5" />{{ summary.failed24h }} 次失败
            </span>
          </template>
          <template v-else-if="summary.runs24h > 0">
            <span class="text-faint">·</span>
            <span class="inline-flex items-center gap-1.5"> <span class="dot ok !h-1.5 !w-1.5" />全部正常 </span>
          </template>
          <template v-if="summary.running > 0">
            <span class="text-faint">·</span>
            <span class="inline-flex items-center gap-1.5 text-warn">
              <span class="dot warn !h-1.5 !w-1.5" />{{ summary.running }} 运行中
            </span>
          </template>
          <template v-if="summary.tokens24h.total > 0">
            <span class="text-faint">·</span>
            <span :title="tokenBreakdown(summary.tokens24h)">{{ formatTokens(summary.tokens24h.total) }} tokens</span>
          </template>
        </div>
        <TransitionGroup tag="div" name="item" class="list">
          <div
            v-for="r in hub.rules"
            :key="r.id"
            class="row link flex cursor-pointer items-center gap-4 px-4 py-3"
            @click="editing = JSON.parse(JSON.stringify(r))"
          >
            <div class="min-w-0 flex-1 transition-opacity duration-200" :class="{ 'opacity-50': !r.enabled }">
              <div class="text-sm font-medium">{{ r.name || '未命名规则' }}</div>
              <div class="truncate text-xs text-mute">{{ describe(r) }}</div>
              <div
                v-if="r.stats && (r.stats.runs_24h > 0 || r.stats.running > 0 || r.stats.last_run_at)"
                class="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-mute tabular-nums"
              >
                <span class="dot !h-1.5 !w-1.5" :class="statsDot(r.stats)" />
                <span v-if="r.stats.running > 0" class="text-warn">{{ r.stats.running }} 运行中</span>
                <span>{{ r.stats.runs_24h > 0 ? `近 24h ${r.stats.runs_24h} 次` : '近 24h 无运行' }}</span>
                <template v-if="r.stats.failed_24h > 0">
                  <span class="text-faint">·</span>
                  <span class="font-medium text-bad">{{ r.stats.failed_24h }} 失败</span>
                </template>
                <template v-if="r.stats.last_run_at">
                  <span class="text-faint">·</span>
                  <span :title="new Date(r.stats.last_run_at).toLocaleString()"
                    >上次 {{ relativeTime(r.stats.last_run_at) }}</span
                  >
                </template>
                <template v-if="r.stats.tokens_24h.total > 0">
                  <span class="text-faint">·</span>
                  <span class="chip tabular-nums" :title="tokenBreakdown(r.stats.tokens_24h)"
                    >{{ formatTokens(r.stats.tokens_24h.total) }} tokens</span
                  >
                </template>
              </div>
              <div v-else class="mt-1 text-xs text-faint">暂无运行记录</div>
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
      </template>
    </div>
    <Transition name="modal">
      <TemplatePicker v-if="choosing" @start="start" @close="choosing = false" />
    </Transition>
    <Transition name="modal">
      <RuleImport v-if="importing" @start="start" @close="importing = false" />
    </Transition>
    <Transition name="drawer">
      <RuleEditor v-if="editing" :rule="editing" @close="editing = null" @saved="onSaved" />
    </Transition>
  </div>
</template>
