// Ready-made rules for the things people most often want. A template says
// what it needs connected (a chat, an AI, something to watch) and, given
// those, builds the rule; the editor then opens on it like on any other.
import type { Component } from 'vue'
import { Bell, Bot, CalendarClock, Layers, PenLine, TriangleAlert, Webhook } from 'lucide-vue-next'
import type { HubEvent, Instance, Plugin, Rule } from './types'
import { SELF } from './editor/picking'

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
const notify: Need = {
  key: 'to',
  title: '发到哪里',
  fits: (p) => (p.category === 'channel' && has(p, 'send')) || p.id === 'notify',
}
const ai: Need = { key: 'ai', title: '交给谁处理', fits: (p) => has(p, 'chat') }

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
    id: 'chat-ai',
    title: '聊天机器人',
    about: '在群里 @ 它或私聊它，交给 AI 回答，答案发回原处',
    icon: Bot,
    needs: [chat, ai],
    build: ({ chat, ai }, plugins) => ({
      ...blank(`${chat.name} → ${ai.name} → 回复`),
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
          action('ai', ai.id, 'chat', params(plugins, ai, 'chat', { prompt: '{{ payload.text }}' })),
          action('reply', '$source', 'reply', params(plugins, chat, 'reply', { text: '{{ steps.ai.text }}' })),
        ],
        edges: [
          { from: 't1', to: 'c1' },
          { from: 'c1', to: 'typing' },
          { from: 'typing', to: 'ai' },
          { from: 'ai', to: 'reply' },
        ],
      },
    }),
  },
  {
    id: 'notify',
    title: '有变化就通知我',
    about: 'RSS 有新文章、网页或接口变了，发到聊天里或弹出本机通知',
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
    id: 'schedule-ai',
    title: '定时让 AI 干活',
    about: '每天几点让 AI 做一件事，比如汇总、巡检，结果发到聊天里',
    icon: CalendarClock,
    needs: [{ key: 'when', title: '什么时候', fits: (p) => p.id === 'schedule' }, ai, notify],
    build: ({ when, ai, to }, plugins) => ({
      ...blank(`${when.name} → ${ai.name} → ${to.name}`),
      graph: {
        nodes: [
          // a time to start from, unless the connection has its own
          trigger(
            't1',
            [when.id],
            [],
            String(when.config?.schedules || '').trim() ? undefined : { schedules: '9:00 在这里写要 AI 做的事' },
          ),
          action('ai', ai.id, 'chat', params(plugins, ai, 'chat', { prompt: '{{ payload.text }}' })),
          action('send', to.id, 'send', params(plugins, to, 'send', { text: '{{ steps.ai.text }}' })),
        ],
        edges: [
          { from: 't1', to: 'ai' },
          { from: 'ai', to: 'send' },
        ],
      },
    }),
  },
  {
    id: 'digest',
    title: '攒一批，让 AI 汇总',
    about: '新文章、新消息先攒着，每天到点让 AI 汇总成一份，发到聊天里',
    icon: Layers,
    needs: [
      { key: 'from', title: '攒什么', fits: (p) => p.capabilities.includes('source') && p.id !== SELF },
      ai,
      notify,
    ],
    build: ({ from, ai, to }, plugins) => ({
      ...blank(`${from.name} → 攒一批 → ${ai.name} → ${to.name}`),
      graph: {
        nodes: [
          trigger('t1', [from.id]),
          action('batch', 'collect', 'add', { item: '{{ payload.text }}', at: '9:00' }),
          action(
            'ai',
            ai.id,
            'chat',
            params(plugins, ai, 'chat', {
              prompt:
                '下面是攒下来的 {{ steps.batch.count }} 条内容，汇总成一份简报，分点列出要点：\n\n{{ steps.batch.text }}',
            }),
          ),
          action('send', to.id, 'send', params(plugins, to, 'send', { text: '{{ steps.ai.text }}' })),
        ],
        edges: [
          { from: 't1', to: 'batch' },
          { from: 'batch', to: 'ai' },
          { from: 'ai', to: 'send' },
        ],
      },
    }),
  },
  {
    id: 'failure',
    title: '规则失败时通知我',
    about: '哪条规则的动作失败了，把原因发到聊天里或弹出本机通知',
    icon: TriangleAlert,
    needs: [notify],
    build: ({ to }, plugins) => ({
      ...blank(`规则失败 → ${to.name}`),
      graph: {
        nodes: [
          trigger('t1', [SELF], ['run.failed']),
          action('send', to.id, 'send', params(plugins, to, 'send', { text: '{{ payload.text }}' })),
        ],
        edges: [{ from: 't1', to: 'send' }],
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
