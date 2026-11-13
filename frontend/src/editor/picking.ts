// What a node is picked from when it is added or changed, whatever its kind:
// a trigger from where events come from, a condition from the ways it can be
// worked out, an action from what can carry it out. Each list offers what
// can be used as it is, what is connected, and what could be connected first,
// which is then set up without leaving the editor.
import { ref, type Component, type ComputedRef } from 'vue'
import { Bot, BrainCircuit, Filter, MessageSquare, Radar, Reply, SquareFunction, Zap } from 'lucide-vue-next'
import { useHub } from '../hub'
import { JUDGE } from '../graph'
import type { Instance, Plugin } from '../types'

// An action sent here replies through whichever instance emitted the event.
export const SOURCE = '$source'
// The ways a condition is worked out that need no instance: rows of fields,
// or an expression. Any other pick is the instance of a model to ask.
export const ROWS = '$rows'
export const EXPR = '$expr'

export const kindTitle: Record<Plugin['category'], string> = { processor: 'AI', channel: '渠道', monitor: '监控' }
export const kindIcon: Record<Plugin['category'], Component> = {
  processor: Bot,
  channel: MessageSquare,
  monitor: Radar,
}

export interface Choice {
  key: string
  name: string
  about: string
  icon: Component
  tag?: string
  value?: string // something usable as it is
  plugin?: Plugin // something to set up first
}
export interface ChoiceGroup {
  title: string
  items: Choice[]
}
interface Picker {
  title: string
  groups: ChoiceGroup[]
  then: (value: string) => void
}

export function usePicking(sourceInstances: ComputedRef<Instance[]>, targetIcon: (instance: string) => Component) {
  const hub = useHub()
  const picker = ref<null | Picker>(null)
  const connecting = ref<null | { plugin: Plugin; then: (instance: string) => void }>(null)
  // Set up an instance of a plugin, then go on with it.
  function connect(plugin: Plugin, then: (instance: string) => void) {
    connecting.value = { plugin, then }
  }

  const does = (p?: Plugin) => p?.actions?.map((a) => a.title).join('、') || ''
  const emits = (p?: Plugin) => p?.events?.map((e) => e.title).join('、') || ''
  const order = ['processor', 'channel', 'monitor']
  // The plugins that could be connected: those with something to set up.
  const connectable = (fits: (p: Plugin) => boolean) =>
    hub.plugins
      .filter((p) => !p.singleton && !p.default_instance && fits(p))
      .sort((a, b) => order.indexOf(a.category) - order.indexOf(b.category))
  const fresh = (p: Plugin, about: string): Choice => ({
    key: 'new:' + p.id,
    name: p.name,
    about,
    icon: kindIcon[p.category],
    tag: kindTitle[p.category],
    plugin: p,
  })

  // Where a trigger's events come from: everything, or one instance. The
  // value is the instance, empty for everything.
  function pickSource(title: string, then: (instance: string) => void) {
    const ready: Choice[] = [
      { key: 'any', name: '所有接入', about: '不管事件来自哪个接入，都算', icon: Zap, value: '' },
    ]
    const connected: Choice[] = []
    for (const i of sourceInstances.value) {
      const p = hub.pluginOf(i.plugin)!
      const c = { key: i.id, name: i.name, icon: targetIcon(i.id), value: i.id }
      const about = `会发出：${emits(p) || '（未声明）'}`
      if (hub.given(i)) ready.push({ ...c, about: p.description || about })
      else
        connected.push({ ...c, about: (p.name !== i.name ? p.name + ' · ' : '') + about, tag: kindTitle[p.category] })
    }
    picker.value = {
      title,
      then,
      groups: [
        { title: '直接可用', items: ready },
        { title: '已接入', items: connected },
        {
          title: '接入新的',
          items: connectable((p) => p.capabilities.includes('source')).map((p) =>
            fresh(p, p.description || `接入后会发出：${emits(p)}`),
          ),
        },
      ],
    }
  }

  // How a condition is worked out: by rows, by an expression, or by asking
  // whatever offers a "judge" action.
  function pickCondition(title: string, then: (way: string) => void) {
    const judge = (p?: Plugin) => !!p?.actions?.some((a) => a.name === JUDGE)
    picker.value = {
      title,
      then,
      groups: [
        {
          title: '直接可用',
          items: [
            {
              key: ROWS,
              name: '按字段判断',
              about: '一行一行地比：内容包含什么、是谁发的…',
              icon: Filter,
              value: ROWS,
            },
            {
              key: EXPR,
              name: '表达式',
              about: '写一条 expr 表达式，能用前面动作的输出，也能算出多个选项之一',
              icon: SquareFunction,
              value: EXPR,
            },
          ],
        },
        {
          title: '交给模型判断',
          items: hub.instances
            .filter((i) => judge(hub.pluginOf(i.plugin)))
            .map((i) => ({
              key: i.id,
              name: i.name,
              about: '把问题和选项交给它，由它来判断',
              icon: BrainCircuit,
              value: i.id,
            })),
        },
        {
          title: '接入新的',
          items: connectable(judge).map((p) => fresh(p, `接入后可以让它来判断：${p.description || p.name}`)),
        },
      ],
    }
  }

  // What carries an action out: Claude Code, QQ, an HTTP request.
  function pickAction(title: string, then: (instance: string) => void) {
    const ready: Choice[] = [
      {
        key: SOURCE,
        name: '原路回复',
        about: '回复到事件来的地方，不管它来自哪个渠道',
        icon: Reply,
        value: SOURCE,
      },
    ]
    const connected: Choice[] = []
    for (const i of hub.instances) {
      const p = hub.pluginOf(i.plugin)
      if (!p?.actions?.length) continue
      const c = { key: i.id, name: i.name, icon: targetIcon(i.id), value: i.id }
      if (p.singleton) ready.push({ ...c, about: p.description || does(p) })
      else if (hub.given(i)) ready.push({ ...c, about: does(p), tag: kindTitle[p.category] })
      else
        connected.push({ ...c, about: (p.name !== i.name ? p.name + ' · ' : '') + does(p), tag: kindTitle[p.category] })
    }
    picker.value = {
      title,
      then,
      groups: [
        { title: '直接可用', items: ready },
        { title: '已接入', items: connected },
        {
          title: '接入新的',
          items: connectable((p) => !!p.actions?.length).map((p) =>
            // a monitor's own description is about what it watches; as an action
            // it only offers to be told to check now
            fresh(p, p.category === 'monitor' ? `接入后可以让它：${does(p)}` : p.description || does(p)),
          ),
        },
      ],
    }
  }

  // What was picked is used as it is, or set up first.
  function choose(c: Choice) {
    const then = picker.value!.then
    picker.value = null
    if (c.plugin) connecting.value = { plugin: c.plugin, then }
    else then(c.value!)
  }
  function connected(id: string) {
    const then = connecting.value!.then
    connecting.value = null
    then(id)
  }

  return { picker, connecting, connect, pickSource, pickCondition, pickAction, choose, connected }
}
