<script setup lang="ts">
// The rule editor: a canvas on which the rule is wired up as a graph, a panel
// that edits the node picked there, and a bar of the things done to the rule
// as a whole.
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import {
  ArrowLeft,
  BrainCircuit,
  ClipboardPaste,
  Copy,
  CopyPlus,
  Filter,
  FlaskConical,
  LayoutGrid,
  Play,
  Maximize,
  Redo2,
  Settings2,
  Trash2,
  Undo2,
  Unlink,
  X,
  Zap,
} from 'lucide-vue-next'
import { useHub, clock, summarize } from '../hub'
import { del, post, put } from '../api'
import type { Rule, Run } from '../types'
import { eventTitle } from '../labels'
import { picksFor } from '../picks'
import { ELSE, NODE_H, NODE_W, choicesOf, clipboard, fromRule, outcomes, toRule, type Doc, type ENode } from '../graph'
import { createEditor, provideEditor } from '../editor/context'
import { useGraphEditing } from '../editor/editing'
import { useHistory } from '../editor/history'
import Drawer from './Drawer.vue'
import ContextMenu, { type MenuItem } from './ContextMenu.vue'
import FlowCanvas, { type CanvasNode, type Point } from './FlowCanvas.vue'
import ConnectDialog from './ConnectDialog.vue'
import ActionPanel from './rule/ActionPanel.vue'
import ConditionPanel from './rule/ConditionPanel.vue'
import NodePicker from './rule/NodePicker.vue'
import TriggerPanel from './rule/TriggerPanel.vue'

const props = defineProps<{ rule: Rule }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const hub = useHub()

const declared = new Set(hub.plugins.flatMap((p) => p.events?.map((e) => e.type) || []))

const doc = reactive<Doc>(fromRule(props.rule, declared))
const canvas = ref<InstanceType<typeof FlowCanvas>>()

// What is picked on the canvas: a node (by its id) or a wire (by its key).
// The panel shows for a node.
const sel = ref<string | null>(props.rule.id ? null : (doc.nodes[0]?.id ?? null))
const PANEL_W = 468
const panels = { trigger: TriggerPanel, condition: ConditionPanel, action: ActionPanel }

const editor = createEditor(doc, sel)
provideEditor(editor)
const { node, scopedEvents, testEventId, trial, trialStep, picker, connecting, triggerSchema, specOf } = editor
const { onMove, addNode, connect, splice, removeNode, removeEdge, unlink, removeSelected, paste, copy, tidy } =
  useGraphEditing(editor, canvas)
const { state, settle, undo, redo } = useHistory(doc, () => {
  if (sel.value && !doc.nodes.some((n) => n.id === sel.value)) sel.value = null
})

// ---- adding a node ----
// Every kind of node is first picked from a list: where a trigger's events
// come from, how a condition is worked out, what carries an action out.
const adding = {
  trigger: { name: '触发…', icon: Zap, pick: editor.pickSource, title: '添加触发：事件从哪里来' },
  condition: { name: '条件…', icon: Filter, pick: editor.pickCondition, title: '添加条件：怎么判断' },
  action: { name: '动作…', icon: Play, pick: editor.pickAction, title: '添加动作：用什么来执行' },
}
function add(kind: ENode['kind'], make: (pick: string) => void = (pick) => addNode(kind, undefined, undefined, pick)) {
  adding[kind].pick(adding[kind].title, make)
}

// ---- trying the rule out ----
// Run the rule as it stands, saved or not, for one event that came in, and
// show on each action what became of it.
const trying = ref(false)
const tryBusy = ref(false)
async function tryOut() {
  const id = testEventId.value || scopedEvents.value[0]?.id
  if (!id) return hub.notify('还没有收到过可以用来试的事件', true)
  settle()
  tryBusy.value = true
  const out = await hub.run(() =>
    post<{ run: Run | null; error?: string }>('/rules/try', {
      rule: toRule(doc, props.rule, (t) => !!triggerSchema(t)),
      event_id: id,
    }),
  )
  tryBusy.value = false
  if (!out) return
  trying.value = false
  trial.value = out
  sel.value = null
}
const trialText = computed(() => {
  const t = trial.value
  if (!t) return ''
  if (t.error) return '规则有问题：' + t.error
  if (!t.run) return '这个事件不会触发这条规则：触发节点不接收它，或者条件没通过'
  const failed = t.run.steps.filter((s) => s.error).length
  return failed
    ? `试运行完成：${t.run.steps.length} 步里有 ${failed} 步失败，点红色的节点看原因`
    : `试运行成功：执行了 ${t.run.steps.length} 步，点节点看结果`
})

// ---- the graph on the canvas ----
const nodes = computed<CanvasNode[]>(() =>
  doc.nodes.map<CanvasNode>((n) => {
    const at = { id: n.id, kind: n.kind, x: n.x, y: n.y }
    if (n.kind === 'trigger')
      return { ...at, icon: Zap, label: '触发', title: editor.triggerTitle(n), sub: editor.triggerSummary(n) }
    if (n.kind === 'condition') {
      const ran = trialStep(n.id)
      return {
        ...at,
        icon: n.ask ? BrainCircuit : Filter,
        label: n.ask ? `模型判断 · ${hub.instanceName(n.instance) || '未选择'}` : '条件',
        title: editor.conditionTitle(n),
        sub: editor.verdict(n, ran?.output?.choice),
        outs: outcomes(n),
        bad: (n.ask && (!specOf(n) || !String(n.params?.prompt || '').trim())) || !!ran?.error,
        state: ran ? (ran.error ? 'bad' : 'ok') : undefined,
      }
    }
    return {
      ...at,
      icon: editor.targetIcon(n.instance),
      label: editor.targetName(n),
      title: editor.actionTitle(n),
      sub: editor.actionSummary(n),
      bad: !specOf(n) || !!trialStep(n.id)?.error,
      state: trialStep(n.id) ? (trialStep(n.id)!.error ? 'bad' : 'ok') : undefined,
    }
  }),
)

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
  const nth = Math.max(
    0,
    outcomes(src).findIndex((o) => o.branch === branch),
  )
  const where = at || { x: src.x + NODE_W + 88, y: src.y + nth * (NODE_H + 24) }
  const lead =
    src.kind !== 'condition'
      ? ''
      : branch === ELSE
        ? choicesOf(src).length
          ? '其他情况 '
          : '不满足时 '
        : branch
          ? `「${branch}」时 `
          : '满足时 '
  return makers(
    (name) => `${lead}→ ${name}`,
    (kind, pick) => addNode(kind, where, { id: from, branch }, pick),
  )
}
// One menu entry for each kind of node that can be put after another.
function makers(label: (name: string) => string, make: (kind: ENode['kind'], pick: string) => void): MenuItem[] {
  return (['action', 'condition'] as const).map((kind) => maker(kind, label, make))
}
function maker(
  kind: ENode['kind'],
  label: (name: string) => string,
  make: (kind: ENode['kind'], pick: string) => void,
): MenuItem {
  return { label: label(adding[kind].name), icon: adding[kind].icon, run: () => add(kind, (pick) => make(kind, pick)) }
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
        (kind, pick) => splice(target.edge!, kind, at, pick),
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
  const putHere = (kind: ENode['kind'], pick: string) => addNode(kind, here, undefined, pick)
  show(e, [
    ...makers((name) => `添加：${name}`, putHere),
    maker('trigger', (name) => `添加：${name}`, putHere),
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
  const body = toRule(doc, props.rule, (t) => !!triggerSchema(t))
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
          <button title="再添加一个触发节点：另一批来源和事件，走自己的流程" @click="add('trigger')">
            <Zap :size="14" />触发
          </button>
          <i />
          <button title="添加一个动作节点：交给 AI、发消息、发请求、跑脚本…" @click="add('action')">
            <Play :size="14" />动作
          </button>
          <i />
          <button title="添加一个条件节点：按字段、按表达式，或者交给模型判断" @click="add('condition')">
            <Filter :size="14" />条件
          </button>
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
      :pad-right="node ? PANEL_W : 0"
      @select="sel = $event"
      @move="onMove"
      @connect="connect"
      @spawn="onSpawn"
      @menu="onMenu"
    />
    <p v-if="trial && !node" class="hint trial" :class="{ bad: trial.error || trial.run?.status === 'error' }">
      <span class="dot" :class="trial.error || trial.run?.status === 'error' ? 'bad' : trial.run ? 'ok' : ''" />
      {{ trialText }}
      <button class="ml-1 cursor-pointer text-mute hover:text-ink" aria-label="清除试运行结果" @click="trial = null">
        <X :size="13" />
      </button>
    </p>
    <p v-else-if="!node" class="hint">
      拖动节点右侧的圆点连线，点一下圆点添加下一步 · 右键查看更多操作 · 滚轮平移，{{ mod === '⌘' ? '⌘' : 'Ctrl' }} +
      滚轮缩放
    </p>
    <ContextMenu v-if="menu" v-bind="menu" @close="menu = null" />

    <Teleport to="body">
      <Transition name="modal">
        <NodePicker
          v-if="picker"
          :title="picker.title"
          :groups="picker.groups"
          @pick="editor.choose"
          @close="picker = null"
        />
        <ConnectDialog
          v-else-if="connecting"
          :plugin="connecting.plugin"
          :picks="picksFor(hub.events)"
          ok-text="接入并选用"
          @done="editor.connected"
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
                动作会真的执行：消息会发出去，AI 会被调用，请求会发出。
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
      v-if="node"
      :key="node.id"
      class="pop absolute top-3 right-3 bottom-3 overflow-y-auto p-5"
      :style="{ width: PANEL_W - 24 + 'px', animation: 'row-slide 0.24s var(--ease-glide)' }"
    >
      <component :is="panels[node.kind]" :node="node" @remove="removeNode(node.id)" />
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
