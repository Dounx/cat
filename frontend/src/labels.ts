// What the fields of an event are called in the interface. The expression
// (payload.text) is what rules store and the engine evaluates; the label is
// what people read.
import type { HubEvent, Plugin } from './types'

const known: Record<string, string> = {
  'payload.text': '内容',
  'payload.mentioned': '提到了机器人',
  'payload.title': '标题',
  'payload.link': '链接',
  'payload.url': '地址',
  'payload.content': '全文',
  'payload.author': '作者',
  'payload.date': '日期',
  'payload.time': '时间',
  'payload.weekday': '星期几',
  'actor.id': '发送者 ID',
  'actor.name': '发送者名字',
  'conversation.id': '所在会话的 ID',
  'conversation.name': '所在会话的名字',
  'conversation.kind': '会话类型（群聊 / 私聊）',
  type: '事件类型',
  source: '来自哪个接入',
}

// A field nobody named is still shown by what it is called in the data.
export function fieldLabel(expr: string): string {
  const e = expr.trim()
  if (known[e]) return known[e]
  if (e.startsWith('payload.')) return `内容里的 ${e.slice(8)}`
  return e
}
export const isKnownField = (expr: string) => !!known[expr.trim()]

// "收到消息" for message.created, as the plugin that emits it declares.
export function eventTitle(plugins: Plugin[], ev: Pick<HubEvent, 'type' | 'plugin'>): string {
  const own = plugins.find((p) => p.id === ev.plugin)?.events?.find((e) => e.type === ev.type)
  return own?.title || plugins.flatMap((p) => p.events || []).find((e) => e.type === ev.type)?.title || ev.type
}
