// Ready-made rules for the things people most often want. A template says
// what it needs connected (a chat, an agent, something to watch) and, given
// those, builds the rule; the editor then opens on it like on any other.
import type { Component } from 'vue'
import { Bell, Bot, CalendarClock, PenLine, Webhook } from 'lucide-vue-next'
import type { HubEvent, Instance, Plugin, Rule } from './types'

export interface Need {
  key: string
  title: string // what it is for, as a question: 在哪里聊
  // the plugins that can fill it
  fits: (p: Plugin) => boolean
}
export interface Template {
  id: string
  title: string
  about: string
  icon: Component
  needs: Need[]
  build: (chosen: Record<string, Instance>, plugins: Plugin[]) => Rule
}

const has = (p: Plugin, action: string) => !!p.actions?.some((a) => a.name === action)
const chat: Need = { key: 'chat', title: '在哪里聊', fits: (p) => p.category === 'channel' && has(p, 'reply') }
const notify: Need = { key: 'to', title: '发到哪里', fits: (p) => p.category === 'channel' && has(p, 'send') }
const agent: Need = { key: 'agent', title: '交给谁处理', fits: (p) => has(p, 'chat') }

const blank = (name = ''): Rule => ({ id: '', name, enabled: true, sources: [], types: [], condition: '', steps: [] })
const trigger = (id: string, sources: string[], types: string[] = [], params?: Record<string, unknown>) => ({
  id,
  kind: 'trigger' as const,
  instance: '',
  action: '',
  sources,
  types,
  ...(params ? { params } : {}),
})
const action = (id: string, instance: string, name: string, params: Record<string, unknown>, more = {}) => ({
  id,
  kind: 'action' as const,
  instance,
  action: name,
  params,
  ...more,
})
// The parameters an action starts from: its declared defaults, then ours.
function params(plugins: Plugin[], inst: Instance, name: string, own: Record<string, unknown>) {
  const spec = plugins.find((p) => p.id === inst.plugin)?.actions?.find((a) => a.name === name)
  const out: Record<string, unknown> = {}
  for (const [k, s] of Object.entries(spec?.params?.properties || {})) if (s.default !== undefined) out[k] = s.default
  return { ...out, ...own }
}

export const templates: Template[] = [
  {
    id: 'chat-agent',
    title: '聊天机器人',
    about: '在群里 @ 它或私聊它，交给 Agent 回答，答案发回原处',
    icon: Bot,
    needs: [chat, agent],
    build: ({ chat, agent }, plugins) => ({
      ...blank(`${chat.name} → ${agent.name} → 回复`),
      graph: {
        nodes: [
          trigger('t1', [chat.id], ['message.created']),
          {
            id: 'c1',
            kind: 'condition',
            instance: '',
            action: '',
            condition: 'payload?.mentioned == true',
            meta: {
              builder: {
                mode: 'builder',
                join: 'and',
                rows: [{ field: 'payload.mentioned', op: 'eq', value: 'true' }],
              },
            },
          },
          action('typing', '$source', 'typing', {}, { continue_on_error: true }),
          action('agent', agent.id, 'chat', params(plugins, agent, 'chat', { prompt: '{{ payload.text }}' })),
          action('reply', '$source', 'reply', params(plugins, chat, 'reply', { text: '{{ steps.agent.text }}' })),
        ],
        edges: [
          { from: 't1', to: 'c1' },
          { from: 'c1', to: 'typing' },
          { from: 'typing', to: 'agent' },
          { from: 'agent', to: 'reply' },
        ],
      },
    }),
  },
  {
    id: 'notify',
    title: '有变化就通知我',
    about: 'RSS 有新文章、网页或接口变了，把内容发到聊天里',
    icon: Bell,
    needs: [
      {
        key: 'watch',
        title: '盯着什么',
        fits: (p) => p.category === 'monitor' && p.id !== 'schedule' && !!p.events?.length,
      },
      notify,
    ],
    build: ({ watch, to }, plugins) => ({
      ...blank(`${watch.name} → ${to.name}`),
      graph: {
        nodes: [
          trigger('t1', [watch.id]),
          action('send', to.id, 'send', params(plugins, to, 'send', { text: '{{ payload.text }}' })),
        ],
        edges: [{ from: 't1', to: 'send' }],
      },
    }),
  },
  {
    id: 'schedule-agent',
    title: '定时让 Agent 干活',
    about: '每天几点让 Agent 做一件事，比如汇总、巡检，结果发到聊天里',
    icon: CalendarClock,
    needs: [{ key: 'when', title: '什么时候', fits: (p) => p.id === 'schedule' }, agent, notify],
    build: ({ when, agent, to }, plugins) => ({
      ...blank(`${when.name} → ${agent.name} → ${to.name}`),
      graph: {
        nodes: [
          // a time to start from, unless the connection has its own
          trigger(
            't1',
            [when.id],
            [],
            String(when.config?.schedules || '').trim() ? undefined : { schedules: '9:00 在这里写要 Agent 做的事' },
          ),
          action('agent', agent.id, 'chat', params(plugins, agent, 'chat', { prompt: '{{ payload.text }}' })),
          action('send', to.id, 'send', params(plugins, to, 'send', { text: '{{ steps.agent.text }}' })),
        ],
        edges: [
          { from: 't1', to: 'agent' },
          { from: 'agent', to: 'send' },
        ],
      },
    }),
  },
  {
    id: 'webhook',
    title: '收到 Webhook 就做点什么',
    about: '别的系统发来一个 HTTP 请求时，转发出去或跑一段脚本',
    icon: Webhook,
    needs: [{ key: 'hook', title: '从哪个 Webhook 来', fits: (p) => p.id === 'webhook' }],
    build: ({ hook }) => ({
      ...blank(`${hook.name} → HTTP 请求`),
      graph: {
        nodes: [trigger('t1', [hook.id]), action('s1', 'http', 'request', { method: 'POST', url: '' })],
        edges: [{ from: 't1', to: 's1' }],
      },
    }),
  },
  {
    id: 'blank',
    title: '从空白开始',
    about: '一张只有触发节点的画布，自己搭',
    icon: PenLine,
    needs: [],
    build: () => blank(),
  },
]

// A rule that starts from one event seen: the same source, the same kind.
export function ruleFromEvent(ev: HubEvent, sourceName: string): Rule {
  return {
    ...blank(`${sourceName} → `),
    graph: { nodes: [trigger('t1', [ev.source], [ev.type])], edges: [] },
  }
}
