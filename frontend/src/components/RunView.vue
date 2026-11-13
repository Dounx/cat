<script setup lang="ts">
// One run of a rule, drawn on the rule's graph as it stood then: the way the
// event went through it stands out, and each node it came to shows what it
// was given and what it gave.
import { computed, onBeforeUnmount, onMounted, reactive, ref, watchEffect } from 'vue'
import { useRouter } from 'vue-router'
import { ArrowLeft, Pencil, X } from 'lucide-vue-next'
import { useHub, summarize } from '../hub'
import type { HubEvent, Rule, Run, StepResult, Visit } from '../types'
import { eventTitle } from '../labels'
import { conditionOf, fromRule, type Doc, type ENode } from '../graph'
import { createEditor, provideEditor } from '../editor/context'
import Drawer from './Drawer.vue'
import FlowCanvas, { type CanvasNode } from './FlowCanvas.vue'
import JsonView from './JsonView.vue'

// `run` is kept current by the caller while it is still going; `rule` is the
// rule it ran as.
const props = defineProps<{ run: Run; rule: Rule; event: HubEvent }>()
const emit = defineEmits<{ close: [] }>()
const hub = useHub()
const router = useRouter()

const declared = new Set(hub.plugins.flatMap((p) => p.events?.map((e) => e.type) || []))
const doc = reactive<Doc>(fromRule(props.rule, declared))
const sel = ref<string | null>(null)
const editor = createEditor(doc, sel)
provideEditor(editor)
const { node, trialPath, trialVisits } = editor
watchEffect(() => (editor.trial.value = { run: props.run }))
const PANEL_W = 468

const brief = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v)).replace(/\s+/g, ' ').slice(0, 60)
// An output in a line: its text, or else the first thing it says in words.
const gist = (out: Record<string, unknown>) =>
  brief(out.text ?? Object.values(out).find((v) => typeof v === 'string' && v.trim()) ?? out)
// What a node did each time the run came to it. An action, and a condition
// put to a model, leave a step for every time; the rule being stopped on a
// loop leaves one too, which is not the node's own.
interface Pass {
  visit: Visit
  step?: StepResult
}
function passes(n: ENode): Pass[] {
  const steps = n.kind === 'action' || n.ask ? props.run.steps.filter((s) => s.id === n.id && s.action !== 'loop') : []
  return trialVisits(n.id).map((visit, i) => ({ visit, step: steps[i] }))
}
const failed = (p: Pass) => !!(p.visit.error || p.step?.error)

const nodes = computed<CanvasNode[]>(() =>
  doc.nodes.map((n) => {
    const all = passes(n)
    const last = all[all.length - 1]
    const base = editor.canvasNode(n)
    const times = all.length > 1 ? ` · ${all.length} 次` : ''
    let sub = base.sub
    if (!last) sub = '没有走到'
    else if (n.kind === 'action')
      sub = last.step ? last.step.error || (last.step.output ? gist(last.step.output) : '完成') : '运行中…'
    return {
      ...base,
      sub: last && n.kind !== 'trigger' ? sub + times : sub,
      dim: !last,
      bad: !!last && failed(last),
      state: last ? (failed(last) ? 'bad' : last.step || n.kind !== 'action' ? 'ok' : undefined) : undefined,
    }
  }),
)

const statusText: Record<string, string> = { ok: '成功', error: '失败', running: '运行中' }
const statusDot: Record<string, string> = { ok: 'ok', error: 'bad', running: 'warn' }
const duration = computed(() => {
  if (!props.run.finished_at) return ''
  const ms = new Date(props.run.finished_at).getTime() - new Date(props.run.started_at).getTime()
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`
})
// A rule that went round its loops too many times was stopped.
const stopped = computed(() => props.run.steps.find((s) => s.action === 'loop' && !s.instance)?.error)
const came = computed(() => new Set(trialPath.value.visits.map((v) => v.node)).size)
const current = computed(() => hub.rules.find((r) => r.id === props.run.rule_id))

const picked = computed(() => (node.value ? passes(node.value) : []))
const heading = computed(() => {
  const n = node.value
  if (!n) return ''
  if (n.kind === 'trigger') return `触发 · ${editor.triggerTitle(n)}`
  if (n.kind === 'condition') return n.ask ? `模型判断 · ${hub.instanceName(n.instance)}` : '条件'
  return `${editor.targetName(n)} · ${editor.actionTitle(n)}`
})

// Escape first lets go of the node picked, then closes this view alone, not
// what it was opened from.
function onEscape(e: KeyboardEvent) {
  if (e.key !== 'Escape') return
  e.stopPropagation()
  if (sel.value) sel.value = null
  else emit('close')
}
onMounted(() => window.addEventListener('keydown', onEscape, true))
onBeforeUnmount(() => window.removeEventListener('keydown', onEscape, true))
</script>

<template>
  <Drawer title="" full @close="emit('close')">
    <template #header>
      <header class="flex items-center gap-3 border-b border-line px-3 py-2">
        <button class="btn btn-ghost px-1.5" title="返回记录" aria-label="返回记录" @click="emit('close')">
          <ArrowLeft :size="16" />
        </button>
        <h2 class="min-w-0 truncate text-[15px] font-semibold">{{ run.rule_name }}</h2>
        <span v-if="run.replay" class="chip shrink-0">重放 / 试运行</span>
        <span class="flex shrink-0 items-center gap-1.5 text-xs text-mute">
          <span class="dot" :class="statusDot[run.status]" />{{ statusText[run.status] }}
          <template v-if="duration">· {{ duration }}</template>
          · {{ new Date(run.started_at).toLocaleString() }}
        </span>
        <button
          v-if="current"
          class="btn ml-auto shrink-0"
          title="打开这条规则现在的样子"
          @click="router.push({ path: '/rules', query: { rule: current.id } })"
        >
          <Pencil :size="14" />编辑规则
        </button>
      </header>
    </template>

    <FlowCanvas
      class="min-w-0 flex-1"
      :nodes="nodes"
      :edges="doc.edges"
      :selected="sel"
      :lit="trialPath.lit"
      :pad-right="node ? PANEL_W : 0"
      readonly
      @select="sel = $event"
    />
    <p v-if="!node" class="hint" :class="{ bad: stopped }">
      <span class="dot" :class="statusDot[run.status]" />
      <template v-if="stopped">{{ stopped }}</template>
      <template v-else>亮起的是这次走过的路径，经过 {{ came }} 个节点 · 点节点看它用了什么、得到什么</template>
    </p>

    <aside
      v-if="node"
      :key="node.id"
      class="pop absolute top-3 right-3 bottom-3 overflow-y-auto p-5"
      :style="{ width: PANEL_W - 24 + 'px', animation: 'row-slide 0.24s var(--ease-glide)' }"
    >
      <div class="mb-3 flex items-center gap-1">
        <h3 class="mr-auto truncate text-sm font-semibold">{{ heading }}</h3>
        <button class="btn btn-ghost px-1.5" title="收起（Esc）" aria-label="收起" @click="sel = null">
          <X :size="14" />
        </button>
      </div>

      <template v-if="node.kind === 'condition' && !node.ask">
        <h4 class="label">条件</h4>
        <p class="mb-1 text-sm">{{ editor.conditionTitle(node) }}</p>
        <JsonView v-if="conditionOf(node) !== editor.conditionTitle(node)" :value="conditionOf(node)" />
      </template>

      <p v-if="!picked.length" class="mt-3 rounded-lg bg-sunken px-3 py-2 text-sm text-mute">这次没有走到这个节点。</p>

      <template v-if="node.kind === 'trigger' && picked.length">
        <p class="mb-3 text-sm text-mute">事件从这里进入规则。</p>
        <dl class="mb-3 grid grid-cols-[4rem_1fr] gap-y-1.5 text-sm">
          <dt class="text-mute">时间</dt>
          <dd>{{ new Date(event.time).toLocaleString() }}</dd>
          <dt class="text-mute">来自</dt>
          <dd>{{ hub.instanceName(event.source) }}</dd>
          <dt class="text-mute">事件</dt>
          <dd>
            {{ eventTitle(hub.plugins, event) }} <code class="chip ml-1">{{ event.type }}</code>
          </dd>
          <template v-if="event.actor"
            ><dt class="text-mute">发送者</dt>
            <dd>{{ event.actor.name || event.actor.id }}</dd></template
          >
          <template v-if="typeof event.payload.text === 'string' && summarize(event)"
            ><dt class="text-mute">内容</dt>
            <dd class="whitespace-pre-wrap">{{ event.payload.text }}</dd></template
          >
        </dl>
        <h4 class="label">事件带的数据</h4>
        <JsonView :value="event.payload" />
      </template>

      <template v-else>
        <div v-for="(p, i) in picked" :key="i" class="mt-3 rounded-xl border border-line">
          <div class="flex items-center justify-between gap-3 px-3 py-2 text-sm">
            <span class="min-w-0 truncate font-medium">
              <template v-if="picked.length > 1">第 {{ i + 1 }} 次 · </template>
              <template v-if="node.kind === 'condition'">{{ editor.verdict(node, p.visit) }}</template>
              <template v-else>{{ p.step ? (p.step.error ? '失败' : '完成') : '运行中…' }}</template>
            </span>
            <span class="flex shrink-0 items-center gap-1.5 font-mono text-xs text-mute">
              <span class="dot" :class="failed(p) ? 'bad' : p.step || node.kind === 'condition' ? 'ok' : 'warn'" />
              <template v-if="p.step">{{ p.step.duration_ms }} ms</template>
            </span>
          </div>
          <div v-if="failed(p) || p.step || p.visit.value !== undefined" class="border-t border-line px-3 py-2.5">
            <p v-if="failed(p)" class="rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">
              {{ p.step?.error || p.visit.error }}
            </p>
            <template v-if="node.kind === 'condition' && !node.ask && !p.visit.error">
              <h4 class="label">表达式的结果</h4>
              <JsonView :value="p.visit.value ?? null" />
            </template>
            <template v-if="p.step?.params">
              <h4 class="label" :class="{ 'mt-3': failed(p) }">
                {{ node.kind === 'condition' ? '问了什么' : '参数' }}
              </h4>
              <JsonView :value="p.step.params" />
            </template>
            <template v-if="p.step && !p.step.error">
              <h4 class="label mt-3">{{ node.kind === 'condition' ? '模型的回答' : '输出' }}</h4>
              <JsonView :value="p.step.output ?? null" />
            </template>
          </div>
        </div>
      </template>
    </aside>
  </Drawer>
</template>

<style scoped>
.hint {
  position: absolute;
  bottom: 14px;
  left: 50%;
  display: flex;
  max-width: calc(100% - 26rem);
  align-items: center;
  gap: 8px;
  padding: 6px 12px;
  overflow: hidden;
  border: 1px solid var(--color-line);
  border-radius: 999px;
  background: var(--color-surface);
  box-shadow: var(--card-edge);
  font-size: 12.5px;
  text-overflow: ellipsis;
  white-space: nowrap;
  transform: translateX(-50%);
}
.hint.bad {
  color: var(--color-bad);
}
</style>
