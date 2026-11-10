<script setup lang="ts">
// The rule editor: a canvas on which the rule is wired up as a graph, a panel
// that edits the node picked there, and a bar of the things done to the rule
// as a whole.
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch, type Component } from 'vue'
import {
  ArrowLeft,
  Bot,
  ChevronRight,
  ClipboardPaste,
  Copy,
  CopyPlus,
  Filter,
  FlaskConical,
  LayoutGrid,
  Play,
  Radar,
  Reply,
  Search,
  MessageSquare,
  Maximize,
  Plus,
  Redo2,
  Settings2,
  Timer,
  Trash2,
  Undo2,
  Unlink,
  Wrench,
  X,
  Zap,
} from 'lucide-vue-next'
import { useHub, clock, summarize, timeAgo } from '../hub'
import { del, post, put } from '../api'
import type { ActionSpec, Plugin, Rule, Run } from '../types'
import { eventTitle, fieldLabel } from '../labels'
import { picksFor } from '../picks'
import { commonFields, operators } from '../condition'
import {
  NODE_H,
  NODE_W,
  blankNode,
  clipboard,
  conditionOf,
  edgeKey,
  freshId,
  fromRule,
  layout,
  toRule,
  typesOf,
  upstreamActions,
  wouldLoop,
  type Doc,
  type ENode,
} from '../graph'
import Drawer from './Drawer.vue'
import ContextMenu, { type MenuItem } from './ContextMenu.vue'
import FlowCanvas, { type CanvasNode, type Point } from './FlowCanvas.vue'
import SchemaForm from './SchemaForm.vue'
import ConnectDialog from './ConnectDialog.vue'
import JsonView from './JsonView.vue'
import type { VarGroup } from './VarPicker.vue'

const props = defineProps<{ rule: Rule }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const hub = useHub()

const declared = new Set(hub.plugins.flatMap((p) => p.events?.map((e) => e.type) || []))

const doc = reactive<Doc>(fromRule(props.rule, declared))
const canvas = ref<InstanceType<typeof FlowCanvas>>()

// What is picked on the canvas: a node (by its id) or a wire (by its key).
// The panel shows for a node.
const sel = ref<string | null>(props.rule.id ? null : (doc.nodes[0]?.id ?? null))
const node = computed(() => doc.nodes.find((n) => n.id === sel.value))
const panel = computed(() => !!node.value)
const PANEL_W = 468

// ---- trigger ----
const sourceInstances = computed(() =>
  hub.instances.filter((i) => hub.pluginOf(i.plugin)?.capabilities.includes('source')),
)
const actionInstances = computed(() => hub.instances.filter((i) => hub.pluginOf(i.plugin)?.actions?.length))
const triggers = computed(() => doc.nodes.filter((n) => n.kind === 'trigger'))
// The instances whose events a trigger listens to: the chosen ones, or all.
const listening = (t: ENode) =>
  t.sources.length ? sourceInstances.value.filter((i) => t.sources.includes(i.id)) : sourceInstances.value
// Every instance some trigger of the rule listens to.
const allListening = computed(() => {
  const ids = new Set(triggers.value.flatMap((t) => listening(t).map((i) => i.id)))
  return sourceInstances.value.filter((i) => ids.has(i.id))
})
const eventsOf = (pluginId: string) => hub.pluginOf(pluginId)?.events || []
const typeTitle = (type: string) => hub.plugins.flatMap((p) => p.events || []).find((e) => e.type === type)?.title

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
// A trigger takes everything until told to take only some. Having switched
// to "only some" with nothing ticked yet is a state of the panel alone.
const narrowed = reactive(new Set<string>())
const anySource = (t: ENode) => !t.sources.length && !narrowed.has(t.id + ':from')
const anyType = (t: ENode) => !typesOf(t).length && !narrowed.has(t.id + ':type')
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
const triggerTitle = (t: ENode) => (t.sources.length ? t.sources.map(hub.instanceName).join('、') : '所有接入')
const triggerSummary = (t: ENode) => {
  const types = typesOf(t)
  const what = types.length ? types.map((x) => typeTitle(x) || x).join('、') : '任何事件'
  return Number(t.limit) > 0 ? `${what} · ${t.limit} 秒内一次` : what
}

// ---- conditions ----
const testEventId = ref('')
const testResult = ref<null | { matched: boolean; error?: string }>(null)
const scopedEvents = computed(() => {
  const ids = new Set(allListening.value.map((i) => i.id))
  return hub.events.filter((e) => ids.has(e.source)).slice(0, 30)
})
// The event the editor shows as an example: the one picked for testing, else the latest in scope.
const sampleEvent = computed(() => hub.events.find((e) => e.id === testEventId.value) || scopedEvents.value[0])
const fieldHints = computed(() => {
  const extra = Object.keys(sampleEvent.value?.payload || {}).map((k) => `payload.${k}`)
  return [...new Set([...commonFields, ...extra])]
})
function conditionTitle(n: ENode): string {
  if (n.builder.mode === 'expr') return n.expr.trim() || '总是成立'
  const rows = n.builder.rows.filter((r) => r.field.trim())
  if (!rows.length) return '总是成立'
  return rows
    .map((r) => {
      const op = operators.find((o) => o.id === r.op)
      return [fieldLabel(r.field), op?.label, op?.noValue ? '' : r.value].filter(Boolean).join(' ')
    })
    .join(n.builder.join === 'and' ? ' 且 ' : ' 或 ')
}
// A row's field is chosen by name; one not on the list is typed as it is
// written in the data.
const OTHER = '__other__'
const typedRows = reactive(new Set<object>())
const typedField = (row: { field: string }) => typedRows.has(row) || !fieldHints.value.includes(row.field)
function chooseField(row: { field: string }, value: string) {
  if (value === OTHER) {
    typedRows.add(row)
    row.field = ''
  } else row.field = value
}
// Switching to the expression editor keeps what the builder had compiled.
function showExpression(n: ENode) {
  n.expr = conditionOf(n)
  n.builder.mode = 'expr'
}
watch([sel, () => node.value && conditionOf(node.value), testEventId], () => {
  testResult.value = null
})
async function test(n: ENode) {
  const id = testEventId.value || scopedEvents.value[0]?.id
  if (!id) return hub.notify('还没有可用于测试的事件', true)
  testEventId.value = id
  testResult.value = (await hub.run(() => post('/rules/test', { condition: conditionOf(n), event_id: id }))) || null
}

// ---- throttles ----
const throttleTitle = (n: ENode) => (Number(n.seconds) > 0 ? `${n.seconds} 秒内只放行一次` : '不限制')

// ---- actions ----
// Actions offered for a node. "$source" replies through whichever instance
// emitted the event, so offer what the rule's possible sources can do.
function actionsFor(n: ENode): ActionSpec[] {
  if (n.instance !== '$source') return hub.pluginOf(hub.instanceOf(n.instance)?.plugin || '')?.actions || []
  const seen = new Map<string, ActionSpec>()
  for (const i of allListening.value) for (const a of hub.pluginOf(i.plugin)?.actions || []) seen.set(a.name, a)
  return [...seen.values()]
}
const specOf = (n: ENode) => actionsFor(n).find((a) => a.name === n.action)
const targetName = (n: ENode) => (n.instance === '$source' ? '原路回复' : hub.instanceName(n.instance) || '未选择')
const actionTitle = (n: ENode) => specOf(n)?.title || n.action || '未选择动作'
// A line of what the action is given: its first free-text parameter, which
// says more than a choice such as the HTTP method.
function actionSummary(n: ENode): string {
  const props = specOf(n)?.params?.properties || {}
  const text = Object.entries(n.params || {}).find(([k, v]) => typeof v === 'string' && v.trim() && !props[k]?.enum)
  return text ? String(text[1]).replace(/\s+/g, ' ') : ''
}

// Start an action's parameters from the defaults its schema declares.
function resetParams(n: ENode) {
  const params: Record<string, unknown> = {}
  for (const [key, s] of Object.entries(specOf(n)?.params?.properties || {})) {
    if (s.default !== undefined) params[key] = s.default
  }
  n.params = params
}
// ---- choosing what carries an action out ----
// An action node is told apart by what it uses: Claude Code, QQ, an HTTP
// request. Adding one, or changing what one uses, opens a list of exactly
// those things: what needs no setting up, what is set up, and what could be,
// which is then set up here without leaving the editor.
const SOURCE = '$source'
const kindTitle: Record<Plugin['category'], string> = { processor: 'Agent', channel: '渠道', monitor: '监控' }
const kindIcon: Record<Plugin['category'], Component> = { processor: Bot, channel: MessageSquare, monitor: Radar }
// The picture for whatever an action is sent to.
function targetIcon(instance: string): Component {
  if (instance === SOURCE) return Reply
  const p = hub.pluginOf(hub.instanceOf(instance)?.plugin || '')
  return !p ? Play : p.singleton ? Wrench : kindIcon[p.category]
}
const does = (p?: Plugin) => p?.actions?.map((a) => a.title).join('、') || ''

interface Choice {
  key: string
  name: string
  about: string
  icon: Component
  tag?: string
  instance?: string // something usable as it is
  plugin?: Plugin // something to set up first
}
const picker = ref<null | { title: string; then: (instance: string) => void }>(null)
const search = ref('')
const choices = computed(() => {
  const ready: Choice[] = [
    {
      key: SOURCE,
      name: '原路回复',
      about: '回复到事件来的地方，不管它来自哪个渠道',
      icon: Reply,
      instance: SOURCE,
    },
  ]
  const connected: Choice[] = []
  for (const i of actionInstances.value) {
    const p = hub.pluginOf(i.plugin)!
    const c = { key: i.id, name: i.name, icon: targetIcon(i.id), instance: i.id }
    if (p.singleton) ready.push({ ...c, about: p.description || does(p) })
    else
      connected.push({ ...c, about: (p.name !== i.name ? p.name + ' · ' : '') + does(p), tag: kindTitle[p.category] })
  }
  const order = ['processor', 'channel', 'monitor']
  const more: Choice[] = hub.plugins
    .filter((p) => !p.singleton && p.actions?.length)
    .sort((a, b) => order.indexOf(a.category) - order.indexOf(b.category))
    .map((p) => ({
      key: 'new:' + p.id,
      name: p.name,
      // a monitor's own description is about what it watches; as an action
      // it only offers to be told to check now
      about: p.category === 'monitor' ? `接入后可以让它：${does(p)}` : p.description || does(p),
      icon: kindIcon[p.category],
      tag: kindTitle[p.category],
      plugin: p,
    }))
  const q = search.value.trim().toLowerCase()
  const hit = (c: Choice) => !q || (c.name + c.about + (c.tag || '')).toLowerCase().includes(q)
  return [
    { title: '直接可用', items: ready.filter(hit) },
    { title: '已接入', items: connected.filter(hit) },
    { title: '接入新的', items: more.filter(hit) },
  ].filter((g) => g.items.length)
})
function pickAction(title: string, then: (instance: string) => void) {
  search.value = ''
  picker.value = { title, then }
}
function choose(c: Choice) {
  const then = picker.value!.then
  picker.value = null
  if (c.instance) return then(c.instance)
  connecting.value = { plugin: c.plugin!, then }
}
// Change what a node uses.
function retarget(n: ENode) {
  pickAction('换一个来执行', (instance) => {
    n.instance = instance
    onTarget(n)
  })
}

const connecting = ref<null | { plugin: Plugin; then: (instance: string) => void }>(null)
function connected(id: string) {
  const then = connecting.value!.then
  connecting.value = null
  then(id)
}

function onTarget(n: ENode) {
  const list = actionsFor(n)
  if (!list.some((a) => a.name === n.action)) n.action = list[0]?.name || ''
  resetParams(n)
}

const brief = (v: unknown) =>
  v === undefined || v === null ? '' : (typeof v === 'string' ? v : JSON.stringify(v)).replace(/\s+/g, ' ').slice(0, 40)
const dig = (obj: unknown, path: string) => path.split('.').reduce((o: any, k) => o?.[k], obj)

// What a node's parameters may refer to: the event, and the outputs of the
// actions that run before it. Output fields are not declared anywhere, so they
// are read off the latest run in which the same action produced something.
function variablesFor(n: ENode): VarGroup[] {
  const groups: VarGroup[] = [
    {
      title: '事件',
      // with an event to go by, only what such an event actually carries
      items: fieldHints.value
        .filter((f) => !sampleEvent.value || f === 'payload.text' || dig(sampleEvent.value, f) !== undefined)
        .map((f) => ({ expr: f, label: fieldLabel(f), sample: brief(dig(sampleEvent.value, f)) })),
    },
  ]
  for (const prev of upstreamActions(doc, n.id)) {
    // what it gave in the trial run, else in the latest run of the same action
    let output = trial.value?.run?.steps.find((s) => s.id === prev.id)?.output
    for (const run of output ? [] : hub.runs) {
      output = run.steps?.find((s) => s.action === prev.action && s.output)?.output
      if (output) break
    }
    const keys = Object.keys(output || {})
    const who = targetName(prev)
    groups.push({
      title: `${who} · ${actionTitle(prev)}`,
      items: keys.length
        ? keys.map((k) => ({
            expr: `steps.${prev.id}.${k}`,
            label: k === 'text' ? `${who} 的输出` : `${who} 的 ${k}`,
            sample: brief(output![k]),
          }))
        : [{ expr: `steps.${prev.id}`, label: `${who} 的全部输出`, sample: '试运行一次后可以选具体的值' }],
    })
  }
  return groups
}

// ---- trying the rule out ----
// Run the rule as it stands, saved or not, for one event that came in, and
// show on each action what became of it.
const trying = ref(false)
const tryBusy = ref(false)
const trial = ref<null | { run: Run | null; error?: string }>(null)
async function tryOut() {
  const id = testEventId.value || scopedEvents.value[0]?.id
  if (!id) return hub.notify('还没有收到过可以用来试的事件', true)
  settle()
  tryBusy.value = true
  const out = await hub.run(() =>
    post<{ run: Run | null; error?: string }>('/rules/try', { rule: toRule(doc, props.rule), event_id: id }),
  )
  tryBusy.value = false
  if (!out) return
  trying.value = false
  trial.value = out
  sel.value = null
}
const trialStep = (id: string) => trial.value?.run?.steps.find((s) => s.id === id)
const trialText = computed(() => {
  const t = trial.value
  if (!t) return ''
  if (t.error) return '规则有问题：' + t.error
  if (!t.run) return '这个事件不会触发这条规则：触发节点不接收它，或者条件没通过'
  const failed = t.run.steps.filter((s) => s.error).length
  return failed
    ? `试运行完成：${t.run.steps.length} 个动作里有 ${failed} 个失败，点红色的节点看原因`
    : `试运行成功：执行了 ${t.run.steps.length} 个动作，点节点看结果`
})

// ---- the graph on the canvas ----
const nodes = computed<CanvasNode[]>(() =>
  doc.nodes.map<CanvasNode>((n) => {
    const at = { id: n.id, kind: n.kind, x: n.x, y: n.y }
    if (n.kind === 'trigger') return { ...at, icon: Zap, label: '触发', title: triggerTitle(n), sub: triggerSummary(n) }
    if (n.kind === 'condition') return { ...at, icon: Filter, label: '条件', title: conditionTitle(n) }
    if (n.kind === 'throttle')
      return {
        ...at,
        icon: Timer,
        label: '节流',
        title: throttleTitle(n),
        sub: n.key.trim() ? `按 ${n.key.trim()} 分别计` : '',
      }
    return {
      ...at,
      icon: targetIcon(n.instance),
      label: targetName(n),
      title: actionTitle(n),
      sub: actionSummary(n),
      bad: !specOf(n) || !!trialStep(n.id)?.error,
      state: trialStep(n.id) ? (trialStep(n.id)!.error ? 'bad' : 'ok') : undefined,
    }
  }),
)
function onMove(id: string, x: number, y: number) {
  const at = doc.nodes.find((n) => n.id === id)
  if (at) Object.assign(at, { x, y })
}

// Nudge a new node down until it is clear of the others.
function clear(at: Point): Point {
  const taken = (p: Point) =>
    doc.nodes.some((n) => Math.abs(n.x - p.x) < NODE_W + 16 && Math.abs(n.y - p.y) < NODE_H + 12)
  const p = { x: Math.round(at.x), y: Math.round(at.y) }
  while (taken(p)) p.y += NODE_H + 24
  return p
}
// Add a node, wired from `from` when given, and open it for editing.
function addNode(kind: ENode['kind'], at?: Point, from?: { id: string; branch: string }, instance = ''): ENode {
  const c = canvas.value!.center()
  const p = clear(at || { x: c.x - NODE_W / 2, y: c.y - NODE_H / 2 })
  const n = blankNode(kind, freshId(doc, kind), p.x, p.y)
  if (kind === 'action') {
    n.instance = instance
    const list = actionsFor(n)
    n.action = (list.find((a) => a.name === 'reply') || list[0])?.name || ''
    resetParams(n)
  }
  doc.nodes.push(n)
  if (from) doc.edges.push({ from: from.id, to: n.id, ...(from.branch ? { branch: from.branch } : {}) })
  sel.value = n.id
  return n
}
function connect(from: string, branch: string, to: string) {
  if (from === to || doc.nodes.find((n) => n.id === to)?.kind === 'trigger') return
  if (doc.edges.some((e) => e.from === from && e.to === to && (e.branch || '') === branch)) return
  if (wouldLoop(doc, from, to)) return hub.notify('这样连会形成循环，规则不能绕回前面的节点', true)
  doc.edges.push({ from, to, ...(branch ? { branch } : {}) })
}
// Put a new node in the middle of a wire.
function splice(key: string, kind: ENode['kind'], at: Point, instance?: string) {
  const i = doc.edges.findIndex((e) => edgeKey(e) === key)
  if (i < 0) return
  const [e] = doc.edges.splice(i, 1)
  const n = addNode(
    kind,
    { x: at.x - NODE_W / 2, y: at.y - NODE_H / 2 },
    { id: e.from, branch: e.branch || '' },
    instance,
  )
  doc.edges.push({ from: n.id, to: e.to })
}
function removeNode(id: string) {
  const n = doc.nodes.find((x) => x.id === id)
  if (n?.kind === 'trigger' && triggers.value.length === 1) {
    return hub.notify('至少要留一个触发节点，否则这条规则永远不会运行', true)
  }
  doc.nodes.splice(
    doc.nodes.findIndex((n) => n.id === id),
    1,
  )
  doc.edges = doc.edges.filter((e) => e.from !== id && e.to !== id)
  if (sel.value === id) sel.value = null
}
function removeEdge(key: string) {
  doc.edges = doc.edges.filter((e) => edgeKey(e) !== key)
  if (sel.value === key) sel.value = null
}
function unlink(id: string) {
  doc.edges = doc.edges.filter((e) => e.from !== id && e.to !== id)
}
function removeSelected() {
  if (!sel.value) return
  if (node.value) removeNode(sel.value)
  else removeEdge(sel.value)
}
// A copy of a node placed nearby (or at `at`), under a new id and unwired.
function paste(src: ENode, at?: Point) {
  const p = clear(at || { x: src.x + 28, y: src.y + 28 })
  const n: ENode = { ...JSON.parse(JSON.stringify(src)), id: freshId(doc, src.kind), ...p }
  doc.nodes.push(n)
  sel.value = n.id
}
function copy(n: ENode) {
  clipboard.node = JSON.parse(JSON.stringify(n))
  hub.notify('已复制节点')
}
function tidy() {
  layout(doc)
  nextTick(() => canvas.value?.fit())
}

// ---- undo ----
// Every settled change to the document is kept as a snapshot.
let last = JSON.stringify(doc)
const saved = ref(last)
const past: string[] = []
const future: string[] = []
const state = reactive({ undo: false, redo: false, dirty: false })
let restoring = false
let timer = 0
function settle() {
  clearTimeout(timer)
  const now = JSON.stringify(doc)
  if (now !== last) {
    past.push(last)
    if (past.length > 200) past.shift()
    future.length = 0
    last = now
  }
  Object.assign(state, { undo: past.length > 0, redo: future.length > 0, dirty: last !== saved.value })
}
watch(
  doc,
  () => {
    if (restoring) return
    clearTimeout(timer)
    timer = window.setTimeout(settle, 250)
  },
  { deep: true },
)
function restore(snapshot: string) {
  restoring = true
  Object.assign(doc, JSON.parse(snapshot))
  last = snapshot
  if (sel.value && !doc.nodes.some((n) => n.id === sel.value)) sel.value = null
  nextTick(() => {
    restoring = false
    settle()
  })
}
function undo() {
  settle()
  if (!past.length) return
  future.push(last)
  restore(past.pop()!)
}
function redo() {
  settle()
  if (!future.length) return
  past.push(last)
  restore(future.pop()!)
}

// ---- menus ----
const menu = ref<null | { x: number; y: number; items: MenuItem[] }>(null)
const mod = /Mac|iP(hone|ad)/.test(navigator.platform) ? '⌘' : 'Ctrl+'
function show(e: MouseEvent, items: MenuItem[]) {
  menu.value = { x: e.clientX, y: e.clientY, items }
}
// What may follow a node: offered when a wire is dropped on empty canvas, when
// a port is clicked, and in the node's menu.
function nextItems(from: string, branch: string, at?: Point): MenuItem[] {
  const src = doc.nodes.find((n) => n.id === from)!
  const where = at || { x: src.x + NODE_W + 88, y: src.y + (branch ? NODE_H + 24 : 0) }
  const lead = src.kind !== 'condition' ? '' : branch ? '不满足时 ' : '满足时 '
  return makers(
    (name) => `${lead}→ ${name}`,
    (kind, instance) => addNode(kind, where, { id: from, branch }, instance),
    false,
  )
}
// One menu entry for each kind of node that can be put after another. An
// action first asks what is to carry it out.
// A throttle of its own is the rarer need (a trigger has a rate limit built
// in), so it is offered only in the fuller menus.
function makers(
  label: (name: string) => string,
  make: (kind: ENode['kind'], instance?: string) => void,
  full = true,
): MenuItem[] {
  return [
    { label: label('动作…'), icon: Play, run: () => pickAction('添加动作：用什么来执行', (i) => make('action', i)) },
    { label: label('条件'), icon: Filter, run: () => make('condition') },
    ...(full ? [{ label: label('节流'), icon: Timer, run: () => make('throttle') }] : []),
  ]
}
function onSpawn(from: string, branch: string, at: Point, e: MouseEvent) {
  show(e, nextItems(from, branch, at))
}
function onMenu(target: { node?: string; edge?: string } | null, at: Point, e: MouseEvent) {
  const n = doc.nodes.find((x) => x.id === target?.node)
  if (target?.edge) {
    sel.value = target.edge
    return show(e, [
      ...makers(
        (name) => `插入：${name}`,
        (kind, instance) => splice(target.edge!, kind, at, instance),
      ),
      {
        label: '删除连线',
        icon: Trash2,
        hint: 'Del',
        danger: true,
        divided: true,
        run: () => removeEdge(target.edge!),
      },
    ])
  }
  if (n) {
    const wired = doc.edges.some((x) => x.from === n.id || x.to === n.id)
    return show(e, [
      { label: '配置', icon: Settings2, run: () => (sel.value = n.id) },
      { label: '复制', icon: Copy, hint: mod + 'C', divided: true, run: () => copy(n) },
      { label: '创建副本', icon: CopyPlus, hint: mod + 'D', run: () => paste(n) },
      { label: '断开所有连线', icon: Unlink, disabled: !wired, run: () => unlink(n.id) },
      { label: '删除', icon: Trash2, hint: 'Del', danger: true, divided: true, run: () => removeNode(n.id) },
    ])
  }
  const here = { x: at.x - NODE_W / 2, y: at.y - NODE_H / 2 }
  show(e, [
    ...makers(
      (name) => `添加：${name}`,
      (kind, instance) => addNode(kind, here, undefined, instance),
    ),
    { label: '添加：触发', icon: Zap, run: () => addNode('trigger', here) },
    {
      label: '粘贴',
      icon: ClipboardPaste,
      hint: mod + 'V',
      disabled: !clipboard.node,
      run: () => paste(clipboard.node!, here),
    },
    { label: '撤销', icon: Undo2, hint: mod + 'Z', divided: true, disabled: !state.undo, run: undo },
    { label: '重做', icon: Redo2, hint: mod + '⇧Z', disabled: !state.redo, run: redo },
    { label: '整理布局', icon: LayoutGrid, divided: true, run: tidy },
    { label: '适应窗口', icon: Maximize, run: () => canvas.value?.fit() },
  ])
}

// ---- keyboard ----
function onKey(e: KeyboardEvent) {
  if (connecting.value || picker.value || trying.value) return
  const cmd = e.metaKey || e.ctrlKey
  const key = e.key.toLowerCase()
  if (cmd && key === 's') {
    e.preventDefault()
    return save()
  }
  // typing in a field keeps its own keys, undo included
  const t = e.target as HTMLElement
  if (menu.value || t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)) return
  if (e.key === 'Delete' || e.key === 'Backspace') removeSelected()
  else if (cmd && key === 'z') e.shiftKey ? redo() : undo()
  else if (cmd && key === 'y') redo()
  else if (cmd && key === 'd' && node.value) paste(node.value)
  else if (cmd && key === 'c' && node.value) copy(node.value)
  else if (cmd && key === 'v' && clipboard.node) paste(clipboard.node)
  else return
  e.preventDefault()
}
// Escape first lets go of what is picked; only with nothing picked does it
// reach the drawer and close the editor.
function onEscape(e: KeyboardEvent) {
  if (e.key === 'Escape' && (connecting.value || picker.value || trying.value)) {
    // the dialog on top takes it; a wizard is left by its own buttons
    e.stopPropagation()
    picker.value = null
    trying.value = false
    if (!connecting.value?.plugin.setup) connecting.value = null
    return
  }
  if (e.key !== 'Escape' || !sel.value || menu.value) return
  e.stopPropagation()
  ;(document.activeElement as HTMLElement | null)?.blur?.()
  sel.value = null
}
onMounted(() => {
  window.addEventListener('keydown', onEscape, true)
  window.addEventListener('keydown', onKey)
})
onBeforeUnmount(() => {
  clearTimeout(timer)
  window.removeEventListener('keydown', onEscape, true)
  window.removeEventListener('keydown', onKey)
})

// ---- the rule as a whole ----
function close() {
  settle()
  if (state.dirty && !confirm('有未保存的修改，确定要关闭吗？')) return
  emit('close')
}
async function save() {
  settle()
  const body = toRule(doc, props.rule)
  const ok = await hub.run(
    () => (props.rule.id ? put(`/rules/${props.rule.id}`, body) : post('/rules', body)),
    '规则已保存',
  )
  if (ok) emit('saved')
}
async function remove() {
  if (!confirm(`删除规则「${doc.name}」？`)) return
  await hub.run(() => del(`/rules/${props.rule.id}`), '已删除')
  emit('saved')
}

// Put the cursor in a field as soon as it shows.
const vFocus = { mounted: (el: HTMLElement) => el.focus() }
</script>

<template>
  <Drawer title="" full @close="close">
    <template #header>
      <header class="flex items-center gap-3 border-b border-line px-3 py-2">
        <div class="flex min-w-0 flex-1 items-center gap-2">
          <button class="btn btn-ghost px-1.5" title="返回规则列表" aria-label="返回规则列表" @click="close">
            <ArrowLeft :size="16" />
          </button>
          <input v-model="doc.name" class="name" placeholder="未命名规则" aria-label="规则名称" />
          <span v-if="state.dirty" class="shrink-0 text-xs text-mute">未保存</span>
        </div>

        <div class="tools">
          <button :disabled="!state.undo" :title="`撤销（${mod}Z）`" aria-label="撤销" @click="undo">
            <Undo2 :size="15" />
          </button>
          <button :disabled="!state.redo" :title="`重做（${mod}⇧Z）`" aria-label="重做" @click="redo">
            <Redo2 :size="15" />
          </button>
          <i />
          <button title="再添加一个触发节点：另一批来源和事件，走自己的流程" @click="addNode('trigger')">
            <Zap :size="14" />触发
          </button>
          <i />
          <button
            title="添加一个动作节点：交给 Agent、发消息、发请求、跑脚本…"
            @click="pickAction('添加动作：用什么来执行', (i) => addNode('action', undefined, undefined, i))"
          >
            <Play :size="14" />动作
          </button>
          <i />
          <button title="添加一个条件节点" @click="addNode('condition')"><Filter :size="14" />条件</button>
          <i />
          <button title="自动排列所有节点" @click="tidy"><LayoutGrid :size="14" />整理</button>
          <i />
          <button title="不用保存，拿一个收到过的事件把这条规则跑一遍" @click="trying = true">
            <FlaskConical :size="14" />试运行
          </button>
        </div>

        <div class="flex flex-1 items-center justify-end gap-2">
          <label class="mr-1 flex cursor-pointer items-center gap-2 text-sm text-mute"
            ><input v-model="doc.enabled" type="checkbox" />启用</label
          >
          <button
            v-if="rule.id"
            class="btn btn-ghost btn-danger px-1.5"
            title="删除规则"
            aria-label="删除规则"
            @click="remove"
          >
            <Trash2 :size="15" />
          </button>
          <button class="btn btn-primary" :title="`保存（${mod}S）`" @click="save">保存</button>
        </div>
      </header>
    </template>

    <FlowCanvas
      ref="canvas"
      class="min-w-0 flex-1"
      :nodes="nodes"
      :edges="doc.edges"
      :selected="sel"
      :pad-right="panel ? PANEL_W : 0"
      @select="sel = $event"
      @move="onMove"
      @connect="connect"
      @spawn="onSpawn"
      @menu="onMenu"
    />
    <p v-if="trial && !panel" class="hint trial" :class="{ bad: trial.error || trial.run?.status === 'error' }">
      <span class="dot" :class="trial.error || trial.run?.status === 'error' ? 'bad' : trial.run ? 'ok' : ''" />
      {{ trialText }}
      <button class="ml-1 cursor-pointer text-mute hover:text-ink" aria-label="清除试运行结果" @click="trial = null">
        <X :size="13" />
      </button>
    </p>
    <p v-else-if="!panel" class="hint">
      拖动节点右侧的圆点连线，点一下圆点添加下一步 · 右键查看更多操作 · 滚轮平移，{{ mod === '⌘' ? '⌘' : 'Ctrl' }} +
      滚轮缩放
    </p>
    <ContextMenu v-if="menu" v-bind="menu" @close="menu = null" />

    <Teleport to="body">
      <Transition name="modal">
        <div
          v-if="picker"
          class="scrim fixed inset-0 z-50 flex items-start justify-center p-4 pt-[12vh]"
          @click.self="picker = null"
        >
          <section class="pop flex max-h-[70vh] w-full max-w-lg flex-col overflow-hidden">
            <header class="flex items-center gap-2 border-b border-line px-4 py-2.5">
              <Search :size="15" class="shrink-0 text-mute" />
              <input
                v-model="search"
                v-focus
                class="min-w-0 flex-1 bg-transparent py-1 text-sm !outline-none placeholder:text-mute/70"
                :placeholder="picker.title + '，可以输入名字查找'"
                @keydown.enter="choices[0] && choose(choices[0].items[0])"
              />
              <button class="btn btn-ghost px-1.5" aria-label="关闭" @click="picker = null"><X :size="16" /></button>
            </header>
            <div class="flex-1 overflow-y-auto p-2">
              <template v-for="g in choices" :key="g.title">
                <div class="px-3 pt-2 pb-1 text-[11px] font-medium text-mute">{{ g.title }}</div>
                <button v-for="c in g.items" :key="c.key" class="choice" @click="choose(c)">
                  <span class="glyph"><component :is="c.icon" :size="15" /></span>
                  <span class="min-w-0 flex-1">
                    <span class="flex items-center gap-2 text-sm font-medium">
                      {{ c.name }}<span v-if="c.tag" class="chip !font-sans">{{ c.tag }}</span>
                    </span>
                    <span class="block truncate text-xs text-mute">{{ c.about }}</span>
                  </span>
                  <span v-if="c.plugin" class="shrink-0 text-xs text-accent">接入</span>
                </button>
              </template>
              <p v-if="!choices.length" class="px-3 py-6 text-center text-sm text-mute">没有找到「{{ search }}」</p>
            </div>
          </section>
        </div>
        <ConnectDialog
          v-else-if="connecting"
          :plugin="connecting.plugin"
          :picks="picksFor(hub.events)"
          ok-text="接入并选用"
          @done="connected"
          @close="connecting = null"
        />
        <div
          v-else-if="trying"
          class="scrim fixed inset-0 z-50 flex items-center justify-center p-4"
          @click.self="trying = false"
        >
          <section class="pop w-full max-w-md overflow-hidden">
            <header class="flex items-center justify-between border-b border-line px-5 py-3">
              <h2 class="text-[15px] font-semibold">试运行</h2>
              <button class="btn btn-ghost px-1.5" aria-label="关闭" @click="trying = false"><X :size="16" /></button>
            </header>
            <div class="space-y-3 p-5">
              <p class="text-sm text-mute">拿一个收到过的事件，把这条规则现在的样子跑一遍，不用先保存。</p>
              <div>
                <label class="label">用哪个事件</label>
                <select v-if="scopedEvents.length" v-model="testEventId" class="input">
                  <option value="">最近的一个</option>
                  <option v-for="e in scopedEvents" :key="e.id" :value="e.id">
                    {{ clock(e.time) }} · {{ eventTitle(hub.plugins, e) }} · {{ summarize(e).slice(0, 50) }}
                  </option>
                </select>
                <p v-else class="text-sm text-mute">触发节点选的接入还没有收到过事件。先让它来一个，比如发一条消息。</p>
              </div>
              <p class="rounded-lg bg-warn/10 px-3 py-2 text-xs text-warn">
                动作会真的执行：消息会发出去，Agent 会被调用，请求会发出。
              </p>
            </div>
            <footer class="flex justify-end gap-2 border-t border-line px-5 py-3">
              <button class="btn" @click="trying = false">取消</button>
              <button class="btn btn-primary" :disabled="tryBusy || !scopedEvents.length" @click="tryOut">
                {{ tryBusy ? '运行中…' : '运行' }}
              </button>
            </footer>
          </section>
        </div>
      </Transition>
    </Teleport>

    <aside
      v-if="panel"
      :key="sel!"
      class="pop absolute top-3 right-3 bottom-3 overflow-y-auto p-5"
      :style="{ width: PANEL_W - 24 + 'px', animation: 'row-slide 0.24s var(--ease-glide)' }"
    >
      <template v-if="node?.kind === 'trigger'">
        <div class="mb-3 flex items-center gap-1">
          <h3 class="mr-auto truncate text-sm font-semibold">触发：什么事件会启动这条规则</h3>
          <button
            v-if="triggers.length > 1"
            class="btn btn-ghost btn-danger px-1.5"
            title="删除节点（Del）"
            aria-label="删除节点"
            @click="removeNode(node.id)"
          >
            <Trash2 :size="14" />
          </button>
          <button class="btn btn-ghost px-1.5" title="收起（Esc）" aria-label="收起" @click="sel = null">
            <X :size="14" />
          </button>
        </div>
        <label class="label">从哪里来</label>
        <div class="seg mb-2">
          <button :class="{ on: anySource(node) }" @click="widen(node, 'from')">所有接入</button>
          <button :class="{ on: !anySource(node) }" @click="narrowed.add(node.id + ':from')">只听指定的</button>
        </div>
        <template v-if="!anySource(node)">
          <div v-if="sourceInstances.length" class="list">
            <label
              v-for="s in sourceInstances"
              :key="s.id"
              class="row link flex cursor-pointer items-center gap-3 px-3 py-2"
            >
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
        </template>
        <p v-if="!sourceInstances.length" class="text-xs text-mute">
          还没有能发出事件的接入，请先到「接入」添加渠道或监控。
        </p>

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
                      >最近一次（{{ timeAgo(e.last.time) }}）：{{
                        summarize(e.last).slice(0, 80) || '（无内容）'
                      }}</template
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
          <p v-if="!typesOf(node).length" class="mt-1 text-xs text-warn">
            还没有选。一个都不选时，按「任何事件」处理。
          </p>
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
            <option v-if="!limitKeys.some((k) => k.value === node!.limitKey)" :value="node.limitKey">
              按 {{ fieldLabel(node.limitKey) }} 分别算
            </option>
          </select>
        </div>
      </template>

      <template v-else-if="node?.kind === 'condition'">
        <div class="mb-3 flex items-center gap-1">
          <h3 class="mr-auto truncate text-sm font-semibold">条件</h3>
          <div class="seg mr-1">
            <button
              class="!py-0.5 !text-xs"
              :class="{ on: node.builder.mode === 'builder' }"
              @click="node.builder.mode = 'builder'"
            >
              可视化
            </button>
            <button
              class="!py-0.5 !text-xs"
              :class="{ on: node.builder.mode === 'expr' }"
              @click="showExpression(node)"
            >
              表达式
            </button>
          </div>
          <button
            class="btn btn-ghost btn-danger px-1.5"
            title="删除节点（Del）"
            aria-label="删除节点"
            @click="removeNode(node.id)"
          >
            <Trash2 :size="14" />
          </button>
          <button class="btn btn-ghost px-1.5" title="收起（Esc）" aria-label="收起" @click="sel = null">
            <X :size="14" />
          </button>
        </div>
        <p class="mb-3 text-xs text-mute">
          成立时走「是」那一头接出去的节点，不成立时走「否」。后面可以接动作，也可以再接条件。
        </p>

        <template v-if="node.builder.mode === 'builder'">
          <div
            v-for="(row, i) in node.builder.rows"
            :key="i"
            class="mb-2 flex items-center gap-2"
            style="animation: item-in 0.2s var(--ease-glide)"
          >
            <span class="w-5 shrink-0 text-center text-xs text-mute">{{
              i === 0 ? '当' : node.builder.join === 'and' ? '且' : '或'
            }}</span>
            <input
              v-if="typedField(row)"
              v-model="row.field"
              class="input min-w-0 flex-1 font-mono text-xs"
              placeholder="payload.字段名"
            />
            <select
              v-else
              class="input min-w-0 flex-1"
              :value="row.field"
              @change="chooseField(row, ($event.target as HTMLSelectElement).value)"
            >
              <option v-for="f in fieldHints" :key="f" :value="f" :title="f">{{ fieldLabel(f) }}</option>
              <option :value="OTHER">其他字段…</option>
            </select>
            <select v-model="row.op" class="input w-24">
              <option v-for="o in operators" :key="o.id" :value="o.id">{{ o.label }}</option>
            </select>
            <input
              v-if="!operators.find((o) => o.id === row.op)?.noValue"
              v-model="row.value"
              class="input flex-1"
              :placeholder="row.op === 'in' ? '用逗号分隔' : '值'"
            />
            <button class="btn btn-ghost px-1.5" aria-label="删除这一行" @click="node.builder.rows.splice(i, 1)">
              <Trash2 :size="14" />
            </button>
          </div>
          <div class="flex items-center gap-3">
            <button class="btn" @click="node.builder.rows.push({ field: 'payload.text', op: 'contains', value: '' })">
              <Plus :size="14" />添加一行
            </button>
            <select v-if="node.builder.rows.length > 1" v-model="node.builder.join" class="input w-36">
              <option value="and">全部满足</option>
              <option value="or">满足任一</option>
            </select>
            <span v-if="!node.builder.rows.length" class="text-xs text-mute">一行都没有时总是成立。</span>
          </div>
        </template>
        <template v-else>
          <textarea
            v-model="node.expr"
            class="input font-mono text-xs"
            rows="4"
            placeholder='payload.text contains "deploy" and actor.id in ["123", "456"]'
          />
          <p class="mt-1 text-xs text-mute">
            expr 表达式。可用变量：payload、actor、conversation、type、source、raw，以及前面动作的输出
            steps.&lt;标识&gt;。留空表示总是成立。
          </p>
        </template>

        <div class="mt-4 flex items-center gap-2">
          <select v-model="testEventId" class="input min-w-0 flex-1 text-xs">
            <option value="">用最近一个事件测试</option>
            <option v-for="e in scopedEvents" :key="e.id" :value="e.id">
              {{ clock(e.time) }} · {{ eventTitle(hub.plugins, e) }} · {{ summarize(e).slice(0, 60) }}
            </option>
          </select>
          <button class="btn" @click="test(node)">测试</button>
          <Transition name="swap" mode="out-in">
            <span
              v-if="testResult"
              :key="testResult.error ? 'e' : testResult.matched ? 'y' : 'n'"
              class="flex shrink-0 items-center gap-1.5 text-sm font-medium"
            >
              <span class="dot" :class="testResult.error ? 'bad' : testResult.matched ? 'ok' : ''" />
              {{ testResult.error ? '出错' : testResult.matched ? '是' : '否' }}
            </span>
          </Transition>
        </div>
        <p v-if="testResult?.error" class="mt-1 text-xs text-bad">{{ testResult.error }}</p>
      </template>

      <template v-else-if="node?.kind === 'throttle'">
        <div class="mb-3 flex items-center gap-1">
          <h3 class="mr-auto truncate text-sm font-semibold">节流</h3>
          <button
            class="btn btn-ghost btn-danger px-1.5"
            title="删除节点（Del）"
            aria-label="删除节点"
            @click="removeNode(node.id)"
          >
            <Trash2 :size="14" />
          </button>
          <button class="btn btn-ghost px-1.5" title="收起（Esc）" aria-label="收起" @click="sel = null">
            <X :size="14" />
          </button>
        </div>
        <p class="mb-4 text-xs text-mute">
          事件走到这里时，一段时间内只放行第一个，其余的到此为止，后面的节点不会执行。只影响经过它的这条线。
        </p>
        <label class="label">多长时间内只放行一次</label>
        <div class="flex items-center gap-2 text-sm">
          <input v-model="node.seconds" type="number" min="0" class="input w-28" />秒
        </div>
        <label class="label mt-4">分别计算（可选）</label>
        <input
          v-model="node.key"
          class="input font-mono text-xs"
          list="field-hints-throttle"
          placeholder="留空：所有事件共用一个计时"
        />
        <datalist id="field-hints-throttle"><option v-for="f in fieldHints" :key="f" :value="f" /></datalist>
        <p class="mt-1 text-xs text-mute">
          填一个字段，就按它的值各自计时。例如填 <code class="chip">actor.id</code> 表示每个人各自
          {{ Number(node.seconds) || 0 }} 秒内只放行一次。
        </p>
      </template>

      <template v-else-if="node">
        <div class="mb-3 flex items-center gap-1">
          <h3 class="mr-auto truncate text-sm font-semibold">{{ targetName(node) }} · {{ actionTitle(node) }}</h3>
          <button
            class="btn btn-ghost btn-danger px-1.5"
            title="删除节点（Del）"
            aria-label="删除节点"
            @click="removeNode(node.id)"
          >
            <Trash2 :size="14" />
          </button>
          <button class="btn btn-ghost px-1.5" title="收起（Esc）" aria-label="收起" @click="sel = null">
            <X :size="14" />
          </button>
        </div>
        <div class="mb-3 flex items-center gap-2">
          <button
            class="input flex flex-1 cursor-pointer items-center gap-2 text-left"
            title="换一个来执行"
            @click="retarget(node)"
          >
            <component :is="targetIcon(node.instance)" :size="14" class="shrink-0 text-mute" />
            <span class="min-w-0 flex-1 truncate">{{ targetName(node) }}</span>
            <span class="shrink-0 text-xs text-mute">更换</span>
          </button>
          <select v-model="node.action" class="input flex-1" @change="resetParams(node)">
            <option v-for="a in actionsFor(node)" :key="a.name" :value="a.name">{{ a.title }}</option>
          </select>
        </div>
        <p v-if="specOf(node)?.description" class="mb-3 text-xs text-mute">{{ specOf(node)!.description }}</p>
        <SchemaForm
          :model-value="node.params || {}"
          :schema="specOf(node)?.params"
          :variables="variablesFor(node)"
          :picks="picksFor(hub.events, node.instance)"
          templated
          @update:model-value="node.params = $event"
        />
        <details class="mt-3 text-xs text-mute">
          <summary class="cursor-pointer">高级</summary>
          <div class="mt-2 space-y-2">
            <label class="flex cursor-pointer items-center gap-2"
              ><input v-model="node.continue_on_error" type="checkbox" />失败后仍然执行它后面的节点</label
            >
            <p>
              节点标识 <code class="chip">{{ node.id }}</code
              >，后面的节点用 <code class="chip" v-text="`{{ steps.${node.id}.… }}`" /> 引用它的输出。
            </p>
          </div>
        </details>
        <p class="mt-3 text-xs text-mute">用「插入变量」把事件的内容、前面动作的结果放进参数里。</p>
        <template v-if="trialStep(node.id)">
          <h4 class="label mt-4">试运行结果</h4>
          <p v-if="trialStep(node.id)!.error" class="mb-2 rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">
            {{ trialStep(node.id)!.error }}
          </p>
          <JsonView v-else :value="trialStep(node.id)!.output ?? null" />
        </template>
      </template>
    </aside>
  </Drawer>
</template>

<style scoped>
/* The rule's name reads as a title until it is being edited. */
.name {
  width: 100%;
  max-width: 22rem;
  min-width: 0;
  padding: 3px 8px;
  border: 1px solid transparent;
  border-radius: 8px;
  background: transparent;
  font-size: 15px;
  font-weight: 600;
  outline: none;
  transition:
    border-color 0.12s,
    background-color 0.12s;
}
.name::placeholder {
  color: var(--color-faint);
}
.name:hover {
  background: var(--color-sunken);
}
.name:focus {
  border-color: var(--color-accent);
  background: var(--color-surface);
}

/* The tools in the middle of the bar: one pill of quiet buttons. */
.tools {
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 3px;
  border-radius: 10px;
  background: var(--seg-track);
}
.tools > button {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 9px;
  border-radius: 7px;
  font-size: 13px;
  white-space: nowrap;
  color: var(--color-mute);
  cursor: pointer;
  transition:
    color 0.12s,
    background-color 0.12s,
    transform 0.14s var(--ease-snap);
}
.tools > button:hover {
  background: var(--seg-thumb);
  box-shadow: var(--seg-edge);
  color: var(--color-ink);
}
.tools > button:active {
  transform: scale(0.95);
}
.tools > button:disabled {
  opacity: 0.35;
  pointer-events: none;
}
.tools > i {
  width: 1px;
  height: 16px;
  margin: 0 4px;
  background: var(--color-line);
}

.choice {
  display: flex;
  width: 100%;
  align-items: center;
  gap: 12px;
  padding: 7px 12px;
  border-radius: 9px;
  text-align: left;
  cursor: pointer;
  outline: none;
}
.choice:hover,
.choice:focus-visible {
  background: var(--hover);
}
.choice .glyph {
  display: flex;
  width: 28px;
  height: 28px;
  flex: none;
  align-items: center;
  justify-content: center;
  border-radius: 8px;
  background: var(--color-accent-soft);
  color: var(--color-accent);
}

.hint.trial {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 12px;
  border: 1px solid var(--color-line);
  border-radius: 999px;
  background: var(--color-surface);
  box-shadow: var(--card-edge);
  font-size: 12.5px;
  color: var(--color-ink);
  pointer-events: auto;
}
.hint {
  position: absolute;
  bottom: 14px;
  left: 50%;
  max-width: calc(100% - 26rem);
  overflow: hidden;
  font-size: 11.5px;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--color-mute);
  transform: translateX(-50%);
  pointer-events: none;
}
</style>
