<script setup lang="ts">
// What came in and what was done about it, as one list: each event with the
// rules it set off and how they fared. An event's details show every action
// taken for it.
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Check, ChevronRight, Inbox, Plus, RotateCcw, Search, ShieldCheck, Square, Workflow, X } from 'lucide-vue-next'
import { useHub, clock, summarize } from '../hub'
import { get, post } from '../api'
import type { ApprovalRequest, HubEvent, Rule, Run, StepResult } from '../types'
import { eventTitle } from '../labels'
import { formatTokens, stepTokens, tokenBreakdown } from '../metrics'
import PageHeader from '../components/PageHeader.vue'
import Drawer from '../components/Drawer.vue'
import JsonView from '../components/JsonView.vue'
import RunView from '../components/RunView.vue'

const hub = useHub()
const route = useRoute()
const router = useRouter()
const source = ref('')
const ruleId = ref('')
const onlyFailed = ref(false)
const keyword = ref('')
const selected = ref<HubEvent | null>(null)
const selectedRuns = ref<Run[]>([])

const PAGE_SIZE = 80
const pageEvents = ref<HubEvent[]>([])
const pageRuns = ref<Run[]>([])
const hasMore = ref(false)
const loading = ref(false)
const loadingMore = ref(false)
let reqSeq = 0
let searchTimer: number | undefined

const sources = computed(() => hub.instances.filter((i) => hub.pluginOf(i.plugin)?.capabilities.includes('source')))
// The runs each event set off, newest first (combining paged runs with live SSE updates).
const runsOf = computed(() => {
  const byId = new Map<string, Run>()
  for (const r of pageRuns.value) byId.set(r.id, r)
  for (const r of hub.runs) byId.set(r.id, r)
  const m = new Map<string, Run[]>()
  for (const r of byId.values()) {
    const list = m.get(r.event_id) || []
    list.push(r)
    m.set(r.event_id, list)
  }
  for (const list of m.values()) {
    list.sort((a, b) => (a.id < b.id ? 1 : a.id > b.id ? -1 : 0))
  }
  return m
})
// A run that did not come through: an action failed, or the hub went down
// while it was going.
const bad = (r: Run) => r.status === 'error' || r.status === 'interrupted'
const filtering = computed(() => !!(source.value || ruleId.value || onlyFailed.value || keyword.value.trim()))
const filtered = computed(() => pageEvents.value)

async function loadPage(append = false) {
  const seq = ++reqSeq
  if (append) loadingMore.value = true
  else loading.value = true
  try {
    const qs = new URLSearchParams({ with_runs: '1', limit: String(PAGE_SIZE) })
    if (source.value) qs.set('source', source.value)
    if (ruleId.value) qs.set('rule_id', ruleId.value)
    if (onlyFailed.value) qs.set('failed', '1')
    const q = keyword.value.trim()
    if (q) qs.set('q', q)
    if (append && pageEvents.value.length) {
      qs.set('before', pageEvents.value[pageEvents.value.length - 1].id)
    }
    const res = await get<{ events: HubEvent[]; runs: Run[]; has_more: boolean }>(`/events?${qs.toString()}`)
    if (seq !== reqSeq) return
    if (append) {
      const seen = new Set(pageEvents.value.map((e) => e.id))
      pageEvents.value = [...pageEvents.value, ...(res.events || []).filter((e) => !seen.has(e.id))]
      const runMap = new Map(pageRuns.value.map((r) => [r.id, r]))
      for (const r of res.runs || []) runMap.set(r.id, r)
      pageRuns.value = [...runMap.values()]
    } else {
      pageEvents.value = res.events || []
      pageRuns.value = res.runs || []
    }
    hasMore.value = !!res.has_more
  } catch (e: any) {
    if (seq === reqSeq) hub.notify(e.message || '加载记录失败', true)
  } finally {
    if (seq === reqSeq) {
      loading.value = false
      loadingMore.value = false
    }
  }
}

onMounted(() => loadPage(false))
onBeforeUnmount(() => clearTimeout(searchTimer))

watch([source, ruleId, onlyFailed], () => loadPage(false))
watch(keyword, () => {
  clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => loadPage(false), 180)
})
watch(
  () => hub.events[0],
  (ev) => {
    if (!ev || ruleId.value || onlyFailed.value) return
    if (source.value && ev.source !== source.value) return
    const k = keyword.value.trim().toLowerCase()
    if (k && !ev.type.toLowerCase().includes(k) && !JSON.stringify(ev).toLowerCase().includes(k)) {
      return
    }
    if (!pageEvents.value.some((x) => x.id === ev.id)) {
      pageEvents.value = [ev, ...pageEvents.value]
    }
  },
)

const statusText: Record<string, string> = {
  ok: '成功',
  error: '失败',
  running: '运行中',
  interrupted: '被打断',
  cancelled: '已停止',
}
const statusDot: Record<string, string> = {
  ok: 'ok',
  error: 'bad',
  running: 'warn',
  interrupted: 'bad',
  cancelled: '',
}
const cancelling = ref<Record<string, boolean>>({})
async function cancelRun(r: Run) {
  if (cancelling.value[r.id]) return
  cancelling.value = { ...cancelling.value, [r.id]: true }
  try {
    await post(`/runs/${encodeURIComponent(r.id)}/cancel`)
  } catch (e: any) {
    hub.notify(e.message || '停止失败', true)
  } finally {
    const next = { ...cancelling.value }
    delete next[r.id]
    cancelling.value = next
  }
}

const approvalComments = ref<Record<string, string>>({})
const deciding = ref<Record<string, boolean>>({})
const approvalById = computed(() => new Map(hub.approvals.map((a) => [a.id, a])))
const approvalStatusText: Record<ApprovalRequest['status'], string> = {
  pending: '待人工确认',
  approved: '已同意',
  rejected: '已拒绝',
  expired: '已超时',
}
async function decide(req: ApprovalRequest, decision: 'approve' | 'reject') {
  if (deciding.value[req.id]) return
  deciding.value = { ...deciding.value, [req.id]: true }
  try {
    const comment = (approvalComments.value[req.id] || '').trim()
    await hub.decideApproval(req.id, decision, comment)
    delete approvalComments.value[req.id]
    hub.notify(decision === 'approve' ? `已同意「${req.title}」，规则正在继续执行` : `已拒绝「${req.title}」`)
  } catch (e: any) {
    hub.notify(e.message || '处理失败', true)
  } finally {
    const next = { ...deciding.value }
    delete next[req.id]
    deciding.value = next
  }
}
async function openEventById(id: string) {
  if (!id) return
  selected.value =
    pageEvents.value.find((e) => e.id === id) ||
    hub.events.find((e) => e.id === id) ||
    (await get<HubEvent>(`/events/${id}`).catch(() => null)) ||
    null
}
function resumedLabel(r: Run): string {
  const first = r.steps?.[0]
  if (first?.instance === 'delay') return '延时后接着运行'
  if (first?.instance === 'approval') return '人工确认后接着运行'
  if (first?.instance === 'collect') return '攒够后接着运行'
  return '恢复后接着运行'
}
function approvalOfStep(s: StepResult): ApprovalRequest | undefined {
  const id = typeof s.output?.approval_id === 'string' ? s.output.approval_id : ''
  return id ? approvalById.value.get(id) : undefined
}
function heldDot(s: StepResult): string {
  if (s.error) return 'bad'
  if (s.held) {
    if (s.instance === 'approval') {
      const ap = approvalOfStep(s)
      if (!ap || ap.status === 'pending') return 'warn'
      return ap.status === 'approved' ? 'ok' : ''
    }
    if (s.instance === 'delay' || s.instance === 'collect') return 'warn'
  }
  return 'ok'
}

// One line for what became of an event.
function outcome(e: HubEvent): { dot: string; text: string } | null {
  const runs = runsOf.value.get(e.id)
  if (!runs?.length) return null
  const names = [...new Set(runs.map((r) => r.rule_name))]
  const pending = hub.pendingApprovals.find((a) => a.event_id === e.id)
  if (pending) return { dot: 'warn', text: `${pending.rule_name || names.join('、')} · 待人工确认` }
  const worst = runs.find(bad) || runs.find((r) => r.status === 'running') || runs[0]
  if (worst === runs[0] && worst.status === 'ok') {
    const heldStep = worst.steps?.find((s) => s.held)
    if (heldStep) {
      if (heldStep.instance === 'delay') return { dot: 'warn', text: `${names.join('、')} · 延时等待中` }
      if (heldStep.instance === 'approval') {
        const ap = approvalOfStep(heldStep)
        if (ap?.status === 'rejected') return { dot: '', text: `${names.join('、')} · 已拒绝` }
        if (ap?.status === 'expired') return { dot: '', text: `${names.join('、')} · 已超时` }
        return { dot: 'warn', text: `${names.join('、')} · 待人工确认` }
      }
      if (heldStep.instance === 'collect') return { dot: 'warn', text: `${names.join('、')} · 等待凑齐` }
      if (heldStep.instance === 'cooldown') return { dot: '', text: `${names.join('、')} · 冷却跳过` }
    }
  }
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
  if (s.action === 'loop' && !s.instance) return '循环'
  const action = hub.pluginOf(hub.instanceOf(s.instance)?.plugin || '')?.actions?.find((a) => a.name === s.action)
  return `${hub.instanceName(s.instance)} · ${action?.title || s.action}`
}

watch(selected, async (ev) => {
  viewing.value = null
  if (!ev) {
    selectedRuns.value = []
    return
  }
  const fetched = await get<Run[]>(`/runs?event_id=${ev.id}`).catch(() => [])
  if (selected.value?.id !== ev.id) return
  const merged = new Map<string, Run>()
  for (const r of fetched) merged.set(r.id, r)
  for (const r of hub.runs) {
    if (r.event_id === ev.id) merged.set(r.id, r)
  }
  selectedRuns.value = [...merged.values()].sort((a, b) => (a.id < b.id ? 1 : a.id > b.id ? -1 : 0))
})

// A run can be looked at on its rule's graph, as the rule stood then: the
// rule comes with the run when it is asked for by id.
const viewing = ref<null | { id: string; rule: Rule }>(null)
const viewed = computed(() => selectedRuns.value.find((r) => r.id === viewing.value?.id))
async function view(r: Run) {
  const full = await hub.run(() => get<Run>(`/runs/${r.id}`))
  if (full?.rule) viewing.value = { id: r.id, rule: full.rule }
}
// Keep the open drawer's runs current as they stream in.
watch(
  () => hub.lastRun,
  (run) => {
    if (!run || !selected.value || run.event_id !== selected.value.id) return
    const i = selectedRuns.value.findIndex((r) => r.id === run.id)
    if (i >= 0) selectedRuns.value[i] = run
    else selectedRuns.value.unshift(run)
  },
)

function stepText(s: StepResult): string {
  return typeof s.output?.text === 'string' ? s.output.text : ''
}
function stepUsage(s: StepResult): { prompt: number; completion: number } | null {
  const u = s.output?.usage as Record<string, unknown> | undefined
  if (!u || typeof u !== 'object') return null
  const prompt = Number(u.prompt_tokens || 0)
  const completion = Number(u.completion_tokens || 0)
  if (!prompt && !completion) return null
  return { prompt, completion }
}
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
      <section v-if="hub.pendingApprovals.length" class="mb-4" data-testid="pending-approvals">
        <div class="mb-2 flex items-center gap-2">
          <ShieldCheck :size="15" class="text-warn" />
          <h2 class="text-xs font-semibold tracking-wide text-ink">待人工确认</h2>
          <span
            class="inline-flex h-4 min-w-4 items-center justify-center rounded-full bg-warn px-1.5 text-[10px] font-semibold text-white tabular-nums"
            >{{ hub.pendingApprovals.length }}</span
          >
        </div>
        <TransitionGroup tag="div" name="item" class="space-y-2.5">
          <div
            v-for="req in hub.pendingApprovals"
            :key="req.id"
            class="card border-warn/45 px-4 py-3.5"
            data-testid="approval-card"
          >
            <div class="flex flex-wrap items-center justify-between gap-2 text-xs text-mute">
              <span class="flex items-center gap-2">
                <span class="dot warn" />
                <span class="font-medium text-ink">{{ req.rule_name || req.rule_id }}</span>
                <span class="font-mono tabular-nums">{{ clock(req.created_at) }}</span>
                <span v-if="req.expires_at" class="chip"
                  >超时：{{ clock(req.expires_at) }}（{{
                    req.on_timeout === 'approve' ? '自动同意' : '自动拒绝'
                  }}）</span
                >
              </span>
              <button
                v-if="req.event_id"
                class="btn btn-ghost !px-2 !py-0.5 !text-xs"
                @click="openEventById(req.event_id)"
              >
                查看事件
              </button>
            </div>
            <h3 class="mt-1.5 text-[14.5px] font-semibold text-ink">{{ req.title }}</h3>
            <pre
              v-if="req.detail"
              class="mt-2 max-h-32 overflow-y-auto rounded-lg bg-sunken px-3 py-2 font-sans text-xs whitespace-pre-wrap text-ink/90"
              >{{ req.detail }}</pre>
            <div class="mt-3 flex flex-wrap items-center gap-2">
              <input
                v-model="approvalComments[req.id]"
                class="input min-w-48 flex-1 text-xs"
                placeholder="可选：填写审批备注…"
                :disabled="deciding[req.id]"
                @keydown.enter="decide(req, 'approve')"
              />
              <button class="btn btn-danger" :disabled="deciding[req.id]" @click="decide(req, 'reject')">
                <X :size="14" />拒绝
              </button>
              <button class="btn btn-primary" :disabled="deciding[req.id]" @click="decide(req, 'approve')">
                <Check :size="14" />{{ deciding[req.id] ? '处理中…' : '同意继续' }}
              </button>
            </div>
          </div>
        </TransitionGroup>
      </section>

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
      <div v-if="hasMore" class="mt-4 flex justify-center">
        <button class="btn" :disabled="loadingMore" @click="loadPage(true)">
          {{ loadingMore ? '加载中…' : '加载更多' }}
        </button>
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
          <template v-if="Array.isArray(selected.payload?.attachments) && selected.payload.attachments.length">
            <dt class="text-mute">附件</dt>
            <dd class="flex flex-col gap-2">
              <div
                v-for="(att, idx) in selected.payload.attachments"
                :key="att?.rel || att?.path || idx"
                class="flex flex-col gap-1"
              >
                <a
                  v-if="att?.rel && (att.kind === 'image' || String(att.mime || att.type || '').startsWith('image/'))"
                  :href="`/api/blobs/${String(att.rel).split('/').map(encodeURIComponent).join('/')}`"
                  target="_blank"
                  rel="noopener"
                  class="inline-block w-fit overflow-hidden rounded border border-line bg-black/20"
                >
                  <img
                    :src="`/api/blobs/${String(att.rel).split('/').map(encodeURIComponent).join('/')}`"
                    :alt="att.name || 'image'"
                    class="max-h-48 max-w-full object-contain"
                  />
                </a>
                <div class="flex flex-wrap items-center gap-2 text-xs">
                  <a
                    v-if="att?.rel"
                    :href="`/api/blobs/${String(att.rel).split('/').map(encodeURIComponent).join('/')}`"
                    :download="att.name || undefined"
                    target="_blank"
                    rel="noopener"
                    class="inline-flex items-center gap-1.5 rounded border border-line px-2 py-0.5 text-ink hover:border-mute"
                  >
                    <span>{{ att.name || '附件' }}</span>
                    <span v-if="att.size" class="text-mute"
                      >({{
                        att.size < 1024
                          ? `${att.size} B`
                          : att.size < 1048576
                            ? `${(att.size / 1024).toFixed(1)} KB`
                            : `${(att.size / 1048576).toFixed(1)} MB`
                      }})</span
                    >
                  </a>
                  <span v-else class="inline-flex items-center gap-1.5 rounded border border-line px-2 py-0.5 text-ink">
                    <span>{{ att?.name || att?.path || '附件' }}</span>
                    <span v-if="att?.size" class="text-mute"
                      >({{
                        att.size < 1024
                          ? `${att.size} B`
                          : att.size < 1048576
                            ? `${(att.size / 1024).toFixed(1)} KB`
                            : `${(att.size / 1048576).toFixed(1)} MB`
                      }})</span
                    >
                  </span>
                  <span v-if="att?.mime || att?.type" class="text-mute">{{ att.mime || att.type }}</span>
                </div>
              </div>
            </dd>
          </template>
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
              >{{ r.rule_name }}<span v-if="r.replay" class="chip ml-2">重放 / 试运行</span
              ><span v-if="r.resumed" class="chip ml-2">{{ resumedLabel(r) }}</span></span
            >
            <span class="flex shrink-0 items-center gap-1.5 text-xs text-mute"
              ><span class="dot" :class="statusDot[r.status]" />{{ statusText[r.status] }}
              <template v-if="duration(r)">· {{ duration(r) }}</template>
              <button
                v-if="r.trace?.length"
                class="btn ml-2"
                title="在规则的图上看这次走过的路径和每个节点的结果"
                @click="view(r)"
              >
                <Workflow :size="14" />看流程
              </button></span
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
                <span class="dot" :class="s.running ? 'warn' : heldDot(s)" />
                <template v-if="s.running">流式输出中 · {{ s.duration_ms }} ms</template>
                <template v-else
                  ><template v-if="s.attempts">试了 {{ s.attempts }} 次 · </template
                  ><template v-if="stepTokens(s)"
                    ><span :title="tokenBreakdown(stepTokens(s)!, '本步')"
                      >{{ formatTokens(stepTokens(s)!.total) }} tokens</span
                    >
                    · </template
                  >{{ s.duration_ms }} ms</template
                >
              </span>
            </div>
            <div v-if="s.error" class="mt-2 rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">
              {{ s.error }}
            </div>
            <div
              v-else-if="stepText(s)"
              class="mt-2 rounded-lg border border-line bg-sunken/60 px-3 py-2 text-sm leading-relaxed text-ink"
            >
              <div class="max-h-60 overflow-y-auto whitespace-pre-wrap break-words">
                {{ stepText(s) }}<span v-if="s.running" class="stream-caret" aria-hidden="true" />
              </div>
              <div v-if="!s.running && stepUsage(s)" class="mt-1.5 flex items-center gap-2 text-[11px] text-mute">
                <span class="chip">Token：输入 {{ stepUsage(s)!.prompt }} · 输出 {{ stepUsage(s)!.completion }}</span>
                <span v-if="s.output?.streamed_message_id" class="chip">已流式更新回复</span>
              </div>
            </div>
            <p v-if="s.held" class="mt-1.5 text-xs text-mute">
              {{
                typeof s.output?.note === 'string' && s.output.note
                  ? s.output.note
                  : `已收下，现在攒了 ${s.output?.count ?? '?'} 条。规则先停在这里，后面的等这一批发出时再运行。`
              }}
            </p>
            <div
              v-if="s.held && s.instance === 'approval' && approvalOfStep(s)"
              class="mt-2 rounded-lg border border-line bg-sunken/60 px-3 py-2 text-xs"
            >
              <template v-if="approvalOfStep(s)!.status === 'pending'">
                <div class="flex flex-wrap items-center gap-2">
                  <input
                    v-model="approvalComments[approvalOfStep(s)!.id]"
                    class="input min-w-36 flex-1 bg-surface text-xs"
                    placeholder="可选：填写审批备注…"
                    :disabled="deciding[approvalOfStep(s)!.id]"
                  />
                  <button
                    class="btn btn-danger !px-2.5 !py-1 !text-xs"
                    :disabled="deciding[approvalOfStep(s)!.id]"
                    @click="decide(approvalOfStep(s)!, 'reject')"
                  >
                    <X :size="13" />拒绝
                  </button>
                  <button
                    class="btn btn-primary !px-2.5 !py-1 !text-xs"
                    :disabled="deciding[approvalOfStep(s)!.id]"
                    @click="decide(approvalOfStep(s)!, 'approve')"
                  >
                    <Check :size="13" />同意继续
                  </button>
                </div>
              </template>
              <div v-else class="flex flex-wrap items-center gap-2 text-mute">
                <span class="chip">{{ approvalStatusText[approvalOfStep(s)!.status] }}</span>
                <span v-if="approvalOfStep(s)!.comment">{{ approvalOfStep(s)!.comment }}</span>
                <span v-if="approvalOfStep(s)!.decided_at" class="font-mono tabular-nums">{{
                  clock(approvalOfStep(s)!.decided_at!)
                }}</span>
              </div>
            </div>
            <p
              v-if="r.resumed === s.id && s.instance === 'approval' && s.output"
              class="mt-1.5 flex flex-wrap items-center gap-2 text-xs text-mute"
            >
              <span class="chip">{{ s.output.approved ? '人工确认：已同意' : '人工确认：已拒绝' }}</span>
              <span v-if="typeof s.output.comment === 'string' && s.output.comment">备注：{{ s.output.comment }}</span>
            </p>
            <details class="mt-1.5 text-xs text-mute">
              <summary class="cursor-pointer">用了什么、得到什么</summary>
              <h4 class="label mt-2">参数</h4>
              <JsonView :value="s.params ?? {}" />
              <h4 class="label mt-3">输出</h4>
              <JsonView :value="s.output ?? null" />
            </details>
          </div>
          <div
            v-if="r.status === 'running'"
            class="flex items-center justify-between gap-2 border-t border-line px-4 py-2.5 text-sm text-mute"
          >
            <span class="flex items-center gap-2"><span class="dot warn" />运行中…</span>
            <button class="btn btn-danger" :disabled="cancelling[r.id]" title="停止这次运行" @click="cancelRun(r)">
              <Square :size="12" />{{ cancelling[r.id] ? '正在停止…' : '停止' }}
            </button>
          </div>
          <p v-if="r.status === 'cancelled'" class="border-t border-line px-4 py-2.5 text-sm text-mute">
            已手动停止这次运行。
          </p>
          <p v-if="r.status === 'interrupted'" class="border-t border-line px-4 py-2.5 text-sm text-bad">
            Cat 在这次运行跑完之前退出了，后面的没有再执行。
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
    <Transition name="drawer">
      <RunView
        v-if="selected && viewing && viewed"
        :key="viewing.id"
        :run="viewed"
        :rule="viewing.rule"
        :event="selected"
        @close="viewing = null"
      />
    </Transition>
  </div>
</template>

<style scoped>
.stream-caret {
  display: inline-block;
  width: 2px;
  height: 1em;
  margin-left: 2px;
  vertical-align: -0.15em;
  background: var(--color-accent);
  animation: stream-blink 0.9s steps(2, start) infinite;
}
@keyframes stream-blink {
  to {
    opacity: 0;
  }
}
</style>
