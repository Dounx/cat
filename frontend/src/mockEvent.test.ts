import { describe, expect, it } from 'vitest'
import {
  buildSampleEvent,
  cloneHistoryEvent,
  formatMockEvent,
  parseMockEventJson,
  readMockPayloadText,
  writeMockPayloadText,
} from './mockEvent'

describe('mockEvent helpers', () => {
  it('builds realistic samples for chat, webhook, feed and schedule events', () => {
    const chat = buildSampleEvent('tg-1', 'telegram', 'message.created')
    expect(chat.actor?.id).toBeTruthy()
    expect(chat.conversation?.kind).toBe('group')
    expect(chat.payload.mentioned).toBe(true)

    const hook = buildSampleEvent('wh-1', 'webhook', 'webhook.received')
    expect(hook.source).toBe('wh-1')
    expect(hook.payload.text).toBeTruthy()

    const feed = buildSampleEvent('rss-1', 'rss', 'feed.item')
    expect(feed.payload.title).toBeTruthy()
    expect(feed.payload.link).toBeTruthy()

    const sched = buildSampleEvent('sc-1', 'schedule', 'schedule.fired', new Date(2026, 4, 15, 9, 5))
    expect(sched.payload.date).toBe('2026-05-15')
    expect(sched.payload.time).toBe('09:05')
  })

  it('clones a historical event without id, time or raw', () => {
    const cloned = cloneHistoryEvent({
      id: 'ev1',
      source: 'wh-1',
      plugin: 'webhook',
      type: 'webhook.received',
      time: '2026-11-22T00:00:00Z',
      payload: { text: 'hello', nested: { a: 1 } },
      raw: { huge: true },
    })
    expect((cloned as any).id).toBeUndefined()
    expect((cloned as any).raw).toBeUndefined()
    expect(cloned.payload).toEqual({ text: 'hello', nested: { a: 1 } })
  })

  it('parses mock event JSON and syncs payload.text', () => {
    const raw = formatMockEvent(buildSampleEvent('wh-1', 'webhook', 'webhook.received'))
    const updated = writeMockPayloadText(raw, 'CPU 达到 98%')
    expect(updated).not.toBeNull()
    expect(readMockPayloadText(updated!)).toBe('CPU 达到 98%')

    const parsed = parseMockEventJson(updated!)
    expect(parsed.error).toBeUndefined()
    expect(parsed.event?.payload.text).toBe('CPU 达到 98%')

    const bad = parseMockEventJson('{broken')
    expect(bad.error).toContain('JSON')
  })
})
