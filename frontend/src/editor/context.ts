// What the parts of the rule editor share: the rule being edited, what is
// picked on the canvas, and how a node reads. The canvas draws each node from
// it, and the panel that edits a node fills its fields from it.
import { computed, inject, provide, reactive, ref, type Component, type InjectionKey, type Ref } from 'vue'
import { Play, Reply, Wrench } from 'lucide-vue-next'
import { useHub } from '../hub'
import { commonFields, operators } from '../condition'
import { typesOf, upstreamActions, type Doc, type ENode } from '../graph'
import { fieldLabel, isKnownField } from '../labels'
import type { ActionSpec, Run } from '../types'
import type { VarGroup } from '../components/VarPicker.vue'
import { SOURCE, kindIcon, usePicking } from './picking'

const brief = (v: unknown) =>
  v === undefined || v === null ? '' : (typeof v === 'string' ? v : JSON.stringify(v)).replace(/\s+/g, ' ').slice(0, 40)
const dig = (obj: unknown, path: string) => path.split('.').reduce((o: any, k) => o?.[k], obj)

export function createEditor(doc: Doc, sel: Ref<string | null>) {
  const hub = useHub()
  // The node picked on the canvas; a wire can be picked too, and is not one.
  const node = computed(() => doc.nodes.find((n) => n.id === sel.value))

  // ---- triggers ----
  const sourceInstances = computed(() =>
    hub.instances.filter((i) => hub.pluginOf(i.plugin)?.capabilities.includes('source')),
  )
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
  // A trigger takes everything until told to take only some. Having switched
  // to "only some" with nothing ticked yet is a state of the panel alone,
  // kept here so that it is still so when the node is picked again.
  const narrowed = reactive(new Set<string>())
  const anyType = (t: ENode) => !typesOf(t).length && !narrowed.has(t.id + ':type')
  // What a trigger can ask of its sources, e.g. the routes to subscribe to: the
  // form their plugin gives for it, when the chosen sources are all of one
  // plugin that has one.
  function triggerSchema(t: ENode) {
    const plugins = new Set(t.sources.map((id) => hub.instanceOf(id)?.plugin || ''))
    return plugins.size === 1 ? hub.pluginOf([...plugins][0])?.trigger_params : undefined
  }
  // What it asks for, line by line, to show on the node.
  const asked = (t: ENode) =>
    triggerSchema(t)
      ? Object.values(t.params)
          .filter((v): v is string => typeof v === 'string')
          .flatMap((v) => v.split('\n'))
          .map((line) => line.trim())
          .filter((line) => line && !line.startsWith('#'))
      : []
  const triggerTitle = (t: ENode) => (t.sources.length ? t.sources.map(hub.instanceName).join('、') : '所有接入')
  const triggerSummary = (t: ENode) => {
    const types = typesOf(t)
    const lines = asked(t)
    const of = lines.length ? `${lines[0]}${lines.length > 1 ? ` 等 ${lines.length} 个` : ''} · ` : ''
    const what = of + (types.length ? types.map((x) => typeTitle(x) || x).join('、') : '任何事件')
    return what
  }

  // ---- the events to go by ----
  // The event picked for testing a condition and for trying the rule out;
  // empty means the latest one.
  const testEventId = ref('')
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

  // ---- conditions ----
  function conditionTitle(n: ENode): string {
    if (n.ask)
      return (
        named(String(n.params?.prompt || ''), n)
          .replace(/\s+/g, ' ')
          .trim() || '还没写要判断什么'
      )
    if (n.builder.mode === 'expr' && n.choices.length) return n.expr.trim() || '还没写表达式'
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
  // The rows of a condition whose field is typed rather than chosen by name:
  // like `narrowed`, a state of the panel that outlasts it.
  const typedRows = reactive(new Set<object>())
  // What the model said in the trial run, as the node's ways out are named.
  function verdict(n: ENode, choice: unknown): string {
    if (choice === undefined) return ''
    const said = n.choices.length ? choice : choice === 'yes' ? '是' : '否'
    return `判断为：${said}`
  }

  // ---- actions ----
  // Actions offered for a node. "$source" replies through whichever instance
  // emitted the event, so offer what the rule's possible sources can do.
  function actionsFor(n: ENode): ActionSpec[] {
    if (n.instance !== SOURCE) return hub.pluginOf(hub.instanceOf(n.instance)?.plugin || '')?.actions || []
    const seen = new Map<string, ActionSpec>()
    for (const i of allListening.value) for (const a of hub.pluginOf(i.plugin)?.actions || []) seen.set(a.name, a)
    return [...seen.values()]
  }
  const specOf = (n: ENode) => actionsFor(n).find((a) => a.name === n.action)
  const targetName = (n: ENode) => (n.instance === SOURCE ? '原路回复' : hub.instanceName(n.instance) || '未选择')
  const actionTitle = (n: ENode) => specOf(n)?.title || n.action || '未选择动作'
  // A line of what the action is given: its first free-text parameter, which
  // says more than a choice such as the HTTP method.
  function actionSummary(n: ENode): string {
    const props = specOf(n)?.params?.properties || {}
    const text = Object.entries(n.params || {}).find(([k, v]) => typeof v === 'string' && v.trim() && !props[k]?.enum)
    return text ? named(String(text[1]), n).replace(/\s+/g, ' ') : ''
  }
  // Start an action's parameters from the defaults its schema declares.
  function resetParams(n: ENode) {
    const params: Record<string, unknown> = {}
    for (const [key, s] of Object.entries(specOf(n)?.params?.properties || {})) {
      if (s.default !== undefined) params[key] = s.default
    }
    n.params = params
  }
  // The picture for whatever an action is sent to.
  function targetIcon(instance: string): Component {
    if (instance === SOURCE) return Reply
    const p = hub.pluginOf(hub.instanceOf(instance)?.plugin || '')
    return !p ? Play : p.singleton ? Wrench : kindIcon[p.category]
  }

  // ---- trying the rule out ----
  // What became of the rule the last time it was tried, shown on each node.
  const trial = ref<null | { run: Run | null; error?: string }>(null)
  // A node on a loop may have run several times; the last is the one shown.
  const trialSteps = (id: string) => trial.value?.run?.steps.filter((s) => s.id === id) || []
  const trialStep = (id: string) => trialSteps(id).pop()

  // ---- what a parameter may refer to ----
  // The event, and the outputs of the actions that run before the node.
  // Output fields are not declared anywhere, so they are read off the latest
  // run in which the same action produced something.
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
      let output = trialStep(prev.id)?.output
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
  // A template as the canvas shows it: each placeholder by the name the fields
  // of the panel give it, 「内容」 rather than {{ payload.text }}. One nothing
  // has a name for stays as typed.
  function named(text: string, n: ENode): string {
    if (!text.includes('{{')) return text
    const names = new Map<string, string>()
    for (const g of variablesFor(n)) for (const v of g.items) if (v.label) names.set(v.expr, v.label)
    return text.replace(/\{\{\s*([^{}]+?)\s*\}\}/g, (raw, expr: string) => {
      const label = names.get(expr) || (isKnownField(expr) ? fieldLabel(expr) : '')
      return label ? `「${label}」` : raw
    })
  }

  // ---- the dialogs over the editor ----
  const picking = usePicking(sourceInstances, targetIcon)

  return {
    doc,
    sel,
    node,
    sourceInstances,
    triggers,
    listening,
    allListening,
    eventsOf,
    typeTitle,
    narrowed,
    anyType,
    triggerSchema,
    triggerTitle,
    triggerSummary,
    testEventId,
    scopedEvents,
    fieldHints,
    conditionTitle,
    typedRows,
    verdict,
    actionsFor,
    specOf,
    targetName,
    actionTitle,
    actionSummary,
    resetParams,
    targetIcon,
    trial,
    trialStep,
    trialSteps,
    variablesFor,
    ...picking,
  }
}

export type Editor = ReturnType<typeof createEditor>

const KEY: InjectionKey<Editor> = Symbol('rule-editor')
export const provideEditor = (editor: Editor) => provide(KEY, editor)
// For the components inside the rule editor.
export const useEditor = () => inject(KEY)!
