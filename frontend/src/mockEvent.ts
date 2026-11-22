// Helpers for building, editing, and parsing synthetic mock events in the
// rule editor's trial runner.
import type { HubEvent } from './types'

export interface MockEventInput {
  source: string
  plugin?: string
  type: string
  actor?: { id: string; name?: string; is_bot?: boolean }
  conversation?: { id: string; kind?: string; name?: string }
  payload: Record<string, unknown>
}

const pad2 = (n: number) => String(n).padStart(2, '0')
const weekdays = ['周日', '周一', '周二', '周三', '周四', '周五', '周六']

// Generate realistic starter fields for a given source/plugin/event type so a
// user can try a brand-new rule immediately without waiting for real traffic.
export function buildSampleEvent(source: string, pluginId: string, type: string, now = new Date()): MockEventInput {
  const cleanType = type && !type.includes('*') ? type : defaultEventType(pluginId)
  if (cleanType === 'message.created' || cleanType.startsWith('message.')) {
    return {
      source,
      ...(pluginId ? { plugin: pluginId } : {}),
      type: cleanType,
      actor: { id: 'user_demo', name: '测试用户' },
      conversation: { id: 'conv_demo', kind: 'group', name: '测试群聊' },
      payload: { text: '你好，这是一条模拟测试消息', mentioned: true },
    }
  }
  if (cleanType === 'webhook.received' || pluginId === 'webhook') {
    return {
      source,
      ...(pluginId ? { plugin: pluginId } : {}),
      type: cleanType || 'webhook.received',
      payload: { text: '这是一条模拟 Webhook 事件', level: 'high' },
    }
  }
  if (cleanType === 'feed.item' || pluginId === 'rss' || pluginId === 'rsshub') {
    return {
      source,
      ...(pluginId ? { plugin: pluginId } : {}),
      type: cleanType || 'feed.item',
      payload: {
        title: '示例文章标题',
        link: 'https://example.com/articles/1',
        text: '这是一篇示例订阅条目的摘要内容。',
        author: '示例作者',
      },
    }
  }
  if (cleanType === 'schedule.fired' || pluginId === 'schedule') {
    return {
      source,
      ...(pluginId ? { plugin: pluginId } : {}),
      type: cleanType || 'schedule.fired',
      payload: {
        text: '定时任务触发',
        date: `${now.getFullYear()}-${pad2(now.getMonth() + 1)}-${pad2(now.getDate())}`,
        time: `${pad2(now.getHours())}:${pad2(now.getMinutes())}`,
        weekday: weekdays[now.getDay()],
      },
    }
  }
  if (cleanType === 'run.failed' || pluginId === 'cat') {
    return {
      source: source || 'cat',
      plugin: pluginId || 'cat',
      type: cleanType || 'run.failed',
      payload: { text: '规则「示例规则」运行失败', rule_name: '示例规则', error: '模拟错误' },
    }
  }
  return {
    source,
    ...(pluginId ? { plugin: pluginId } : {}),
    type: cleanType || 'event.test',
    payload: { text: '这是一条模拟事件内容' },
  }
}

export function defaultEventType(pluginId: string): string {
  switch (pluginId) {
    case 'webhook':
      return 'webhook.received'
    case 'rss':
    case 'rsshub':
      return 'feed.item'
    case 'schedule':
      return 'schedule.fired'
    case 'cat':
      return 'run.failed'
    case 'telegram':
    case 'feishu':
    case 'dingtalk':
    case 'discord':
    case 'onebot':
    case 'qqbot':
    case 'wecom':
      return 'message.created'
    default:
      return 'webhook.received'
  }
}

// Clone a historical event into an editable mock event without id/time/raw.
export function cloneHistoryEvent(ev: HubEvent): MockEventInput {
  const out: MockEventInput = {
    source: ev.source,
    ...(ev.plugin ? { plugin: ev.plugin } : {}),
    type: ev.type,
    payload: structuredClone(ev.payload || {}),
  }
  if (ev.actor) out.actor = structuredClone(ev.actor)
  if (ev.conversation) out.conversation = structuredClone(ev.conversation)
  return out
}

export function formatMockEvent(ev: MockEventInput): string {
  return JSON.stringify(ev, null, 2)
}

export function parseMockEventJson(
  raw: string,
  fallback: { source?: string; plugin?: string; type?: string } = {},
): { event?: MockEventInput; error?: string } {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch (e: any) {
    return { error: 'JSON 格式错误：' + (e?.message || String(e)) }
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { error: '模拟事件必须是 JSON 对象（以 { 开头）' }
  }
  const obj = parsed as Record<string, unknown>
  const source = typeof obj.source === 'string' && obj.source.trim() ? obj.source.trim() : fallback.source || ''
  const plugin = typeof obj.plugin === 'string' && obj.plugin.trim() ? obj.plugin.trim() : fallback.plugin || ''
  const type = typeof obj.type === 'string' && obj.type.trim() ? obj.type.trim() : fallback.type || ''
  const payload =
    obj.payload && typeof obj.payload === 'object' && !Array.isArray(obj.payload)
      ? (obj.payload as Record<string, unknown>)
      : {}
  const ev: MockEventInput = {
    ...obj,
    source,
    ...(plugin ? { plugin } : {}),
    type,
    payload,
  }
  return { event: ev }
}

export function readMockPayloadText(raw: string): string {
  try {
    const parsed = JSON.parse(raw)
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      const text = (parsed as any).payload?.text
      return typeof text === 'string' ? text : ''
    }
  } catch {
    // ignore invalid JSON while user is typing
  }
  return ''
}

export function writeMockPayloadText(raw: string, text: string): string | null {
  try {
    const parsed = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null
    const obj = parsed as Record<string, any>
    if (!obj.payload || typeof obj.payload !== 'object' || Array.isArray(obj.payload)) obj.payload = {}
    obj.payload.text = text
    return JSON.stringify(obj, null, 2)
  } catch {
    return null
  }
}
