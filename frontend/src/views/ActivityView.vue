<script setup lang="ts">
// What came in and what was done about it, as one list: each event with the
// rules it set off and how they fared. An event's details show every action
// taken for it.
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ChevronRight, Inbox, Plus, RotateCcw, Search, UserCheck } from 'lucide-vue-next'
import { useHub, clock, summarize } from '../hub'
import { get, post, put } from '../api'
import type { HubEvent, Run, StepResult } from '../types'
import { eventTitle } from '../labels'
import PageHeader from '../components/PageHeader.vue'
import Drawer from '../components/Drawer.vue'
import JsonView from '../components/JsonView.vue'

const hub = useHub()
const route = useRoute()
const router = useRouter()
const source = ref('')
const ruleId = ref('')
const onlyFailed = ref(false)
const keyword = ref('')
const selected = ref<HubEvent | null>(null)
const selectedRuns = ref<Run[]>([])

const sources = computed(() => hub.instances.filter((i) => hub.pluginOf(i.plugin)?.capabilities.includes('source')))
// The runs each event set off, newest first.
const runsOf = computed(() => {
  const m = new Map<string, Run[]>()
  for (const r of hub.runs) m.set(r.event_id, [...(m.get(r.event_id) || []), r])
  return m
})
const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  return hub.events.filter((e) => {
    const runs = runsOf.value.get(e.id) || []
    return (
      (!source.value || e.source === source.value) &&
      (!ruleId.value || runs.some((r) => r.rule_id === ruleId.value)) &&
      (!onlyFailed.value || runs.some((r) => r.status === 'error')) &&
      (!k ||
        eventTitle(hub.plugins, e).toLowerCase().includes(k) ||
        e.type.toLowerCase().includes(k) ||
        JSON.stringify(e.payload).toLowerCase().includes(k) ||
        (e.actor?.name || '').toLowerCase().includes(k))
    )
  })
})
const filtering = computed(() => !!(source.value || ruleId.value || onlyFailed.value || keyword.value.trim()))

const statusText: Record<string, string> = { ok: '成功', error: '失败', running: '运行中' }
const statusDot: Record<string, string> = { ok: 'ok', error: 'bad', running: 'warn' }
// One line for what became of an event.
function outcome(e: HubEvent): { dot: string; text: string } | null {
  const runs = runsOf.value.get(e.id)
  if (!runs?.length) return null
  const worst = runs.find((r) => r.status === 'error') || runs.find((r) => r.status === 'running') || runs[0]
  const names = [...new Set(runs.map((r) => r.rule_name))]
  return { dot: statusDot[worst.status], text: `${names.join('、')} · ${statusText[worst.status]}` }
}
function duration(r: Run): string {
  if (!r.finished_at) return ''
  const ms = new Date(r.finished_at).getTime() - new Date(r.started_at).getTime()
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`
}
// "Claude Code · 对话" for a step, in the words of the plugin behind it.
function stepTitle(s: StepResult): string {
  if (s.action === 'condition' && !s.instance) return '条件'
  const action = hub.pluginOf(hub.instanceOf(s.instance)?.plugin || '')?.actions?.find((a) => a.name === s.action)
  return `${hub.instanceName(s.instance)} · ${action?.title || s.action}`
}

watch(selected, async (ev) => {
  selectedRuns.value = ev ? await get<Run[]>(`/runs?event_id=${ev.id}`).catch(() => []) : []
})
// Keep the open drawer's runs current as they stream in.
watch(
  () => hub.runs[0],
  (run) => {
    if (!run || !selected.value || run.event_id !== selected.value.id) return
    const i = selectedRuns.value.findIndex((r) => r.id === run.id)
    if (i >= 0) selectedRuns.value[i] = run
    else selectedRuns.value.unshift(run)
  },
  { deep: true },
)
// Links from elsewhere open one event, named directly or by a run of it.
watch(
  () => route.query,
  async (q) => {
    let id = typeof q.event === 'string' ? q.event : ''
    if (!id && typeof q.run === 'string') {
      const run = hub.runs.find((r) => r.id === q.run) || (await get<Run>(`/runs/${q.run}`).catch(() => null))
      id = run?.event_id || ''
    }
    if (!id) return
    selected.value =
      hub.events.find((e) => e.id === id) || (await get<HubEvent>(`/events/${id}`).catch(() => null)) || null
    router.replace({ path: '/activity' })
  },
  { immediate: true },
)

async function replay() {
  const out = await hub.run(() => post<{ matched: number }>(`/events/${selected.value!.id}/replay`))
  if (out) hub.notify(out.matched ? `已重放，触发了 ${out.matched} 条规则` : '已重放，没有规则被触发')
}

// An agent that turned a sender away can be told to take them, right where
// the refusal shows.
const refused = (s: StepResult) => !!s.error?.includes('不在白名单') && !!selected.value?.actor?.id
async function allowSender(s: StepResult) {
  const inst = hub.instanceOf(s.instance)
  const actor = selected.value?.actor
  if (!inst || !actor) return
  const list = String(inst.config.allowed_actors || '')
    .split(/[,，\s]+/)
    .filter(Boolean)
  if (!list.includes(actor.id)) list.push(actor.id)
  const ok = await hub.run(
    () =>
      put(`/instances/${inst.id}`, {
        name: inst.name,
        enabled: inst.enabled,
        config: { ...inst.config, allowed_actors: list.join(',') },
      }),
    `已允许 ${actor.name || actor.id} 触发 ${inst.name}，可以重放这个事件再试一次`,
  )
  if (ok) hub.reloadInstances()
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="记录" hint="收到了什么，规则对它做了什么，实时更新">
      <label class="flex cursor-pointer items-center gap-2 text-sm text-mute"
        ><input v-model="onlyFailed" type="checkbox" />只看失败</label
      >
      <div class="relative">
        <Search :size="14" class="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-mute" />
        <input v-model="keyword" class="input w-48 !pl-8" placeholder="搜索内容、发送者" />
      </div>
      <select v-model="source" class="input w-36">
        <option value="">全部接入</option>
        <option v-for="s in sources" :key="s.id" :value="s.id">{{ s.name }}</option>
      </select>
      <select v-model="ruleId" class="input w-36">
        <option value="">全部规则</option>
        <option v-for="r in hub.rules" :key="r.id" :value="r.id">{{ r.name }}</option>
      </select>
    </PageHeader>

    <div class="gutter flex-1 overflow-y-auto pb-6">
      <div v-if="!filtered.length" class="card empty">
        <Inbox :size="28" :stroke-width="1.5" />
        <template v-if="filtering"><p>没有符合筛选条件的记录。</p></template>
        <template v-else>
          <p class="text-ink">还没有收到任何东西</p>
          <p class="text-xs">接入一个渠道或监控之后，它收到的每条消息、发现的每次变化都会出现在这里。</p>
        </template>
      </div>
      <div v-else class="list">
        <table class="w-full table-fixed text-left">
          <TransitionGroup tag="tbody" name="arrive">
            <tr v-for="e in filtered" :key="e.id" class="cursor-pointer" @click="selected = e">
              <td class="w-28 py-2.5 pl-4 font-mono text-xs whitespace-nowrap text-mute tabular-nums">
                {{ clock(e.time) }}
              </td>
              <td class="w-32 truncate py-2.5 pr-3 text-sm font-medium">{{ hub.instanceName(e.source) }}</td>
              <td class="w-28 truncate py-2.5 pr-3 text-xs text-mute" :title="e.type">
                {{ eventTitle(hub.plugins, e) }}
              </td>
              <td class="truncate py-2.5 pr-3 text-sm">
                <span v-if="e.actor?.name" class="mr-1.5 text-mute">{{ e.actor.name }}：</span>{{ summarize(e) }}
              </td>
              <td class="w-56 truncate py-2.5 pr-3 text-xs">
                <span v-if="outcome(e)" class="flex min-w-0 items-center gap-1.5">
                  <span class="dot shrink-0" :class="outcome(e)!.dot" />
                  <span class="truncate">{{ outcome(e)!.text }}</span>
                </span>
                <span v-else class="text-faint">没有触发规则</span>
              </td>
              <td class="w-9 pr-3 text-faint"><ChevronRight :size="15" /></td>
            </tr>
          </TransitionGroup>
        </table>
      </div>
    </div>

    <Transition name="drawer">
      <Drawer v-if="selected" :title="eventTitle(hub.plugins, selected)" wide @close="selected = null">
        <template #actions>
          <button class="btn" title="把这个事件再交给规则处理一遍" @click="replay"><RotateCcw :size="14" />重放</button>
        </template>
        <dl class="mb-4 grid grid-cols-[5rem_1fr] gap-y-1.5 text-sm">
          <dt class="text-mute">时间</dt>
          <dd>{{ new Date(selected.time).toLocaleString() }}</dd>
          <dt class="text-mute">来自</dt>
          <dd>{{ hub.instanceName(selected.source) }}</dd>
          <template v-if="selected.actor"
            ><dt class="text-mute">发送者</dt>
            <dd>
              {{ selected.actor.name || selected.actor.id }} <span class="chip ml-1">{{ selected.actor.id }}</span>
            </dd></template
          >
          <template v-if="selected.conversation"
            ><dt class="text-mute">会话</dt>
            <dd>
              {{ selected.conversation.name || selected.conversation.id }}
              <span v-if="selected.conversation.name" class="chip ml-1">{{ selected.conversation.id }}</span>
            </dd></template
          >
          <template v-if="summarize(selected) && typeof selected.payload.text === 'string'"
            ><dt class="text-mute">内容</dt>
            <dd class="whitespace-pre-wrap">{{ selected.payload.text }}</dd></template
          >
        </dl>

        <h3 class="label">规则对它做了什么</h3>
        <div v-if="!selectedRuns.length" class="card flex items-center gap-3 px-4 py-3 text-sm">
          <span class="min-w-0 flex-1 text-mute">没有规则被它触发。</span>
          <RouterLink :to="{ path: '/rules', query: { event: selected.id } }" class="btn shrink-0">
            <Plus :size="14" />用它建一条规则
          </RouterLink>
        </div>
        <div v-for="r in selectedRuns" :key="r.id" class="mb-3 rounded-xl border border-line">
          <div class="flex items-center justify-between gap-3 px-4 py-2.5 text-sm">
            <span class="min-w-0 truncate font-medium"
              >{{ r.rule_name }}<span v-if="r.replay" class="chip ml-2">重放 / 试运行</span></span
            >
            <span class="flex shrink-0 items-center gap-1.5 text-xs text-mute"
              ><span class="dot" :class="statusDot[r.status]" />{{ statusText[r.status] }}
              <template v-if="duration(r)">· {{ duration(r) }}</template></span
            >
          </div>
          <div v-for="(s, i) in r.steps" :key="s.id + i" class="border-t border-line px-4 py-2.5">
            <div class="flex items-center justify-between gap-3 text-sm">
              <span class="flex min-w-0 items-center gap-2">
                <span
                  class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-sunken text-[11px] text-mute tabular-nums"
                  >{{ i + 1 }}</span
                >
                <span class="truncate">{{ stepTitle(s) }}</span>
              </span>
              <span class="flex shrink-0 items-center gap-1.5 font-mono text-xs text-mute">
                <span class="dot" :class="s.error ? 'bad' : 'ok'" />{{ s.duration_ms }} ms
              </span>
            </div>
            <div v-if="s.error" class="mt-2 rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">
              {{ s.error }}
              <button v-if="refused(s)" class="btn mt-2 flex" @click="allowSender(s)">
                <UserCheck :size="14" />允许 {{ selected.actor!.name || selected.actor!.id }}
              </button>
            </div>
            <details class="mt-1.5 text-xs text-mute">
              <summary class="cursor-pointer">用了什么、得到什么</summary>
              <h4 class="label mt-2">参数</h4>
              <JsonView :value="s.params ?? {}" />
              <h4 class="label mt-3">输出</h4>
              <JsonView :value="s.output ?? null" />
            </details>
          </div>
          <p
            v-if="r.status === 'running'"
            class="flex items-center gap-2 border-t border-line px-4 py-2.5 text-sm text-mute"
          >
            <span class="dot warn" />运行中…
          </p>
        </div>

        <details class="mt-4">
          <summary class="label cursor-pointer">完整数据</summary>
          <p class="mb-2 text-xs text-mute">
            规则里能用到的全部字段。事件类型 <code class="chip">{{ selected.type }}</code
            >，事件 ID <code class="chip">{{ selected.id }}</code>
          </p>
          <JsonView :value="selected.payload" />
          <h4 class="label mt-3">平台发来的原始数据</h4>
          <JsonView :value="selected.raw ?? null" />
        </details>
      </Drawer>
    </Transition>
  </div>
</template>
