import { describe, expect, it } from 'vitest'
import { fromRule, toRule } from './graph'
import { ruleFromEvent, templates } from './templates'
import type { HubEvent, Instance, Plugin } from './types'

// One plugin and one instance of it for whatever a template needs.
const plugin = (id: string, category: Plugin['category'], actions: string[], more: Partial<Plugin> = {}) =>
  ({
    id,
    name: id,
    category,
    capabilities: ['source', 'action'],
    events: [{ type: 'message.created', title: '收到消息' }],
    actions: actions.map((name) => ({
      name,
      title: name,
      params: { type: 'object', properties: { model: { type: 'string', default: 'auto' } } },
    })),
    ...more,
  }) as unknown as Plugin
const plugins = [
  plugin('qq', 'channel', ['reply', 'send', 'typing']),
  plugin('claude', 'processor', ['chat']),
  plugin('rss', 'monitor', ['check']),
  plugin('schedule', 'monitor', []),
  plugin('webhook', 'channel', []),
]
const instance = (p: Plugin): Instance =>
  ({ id: p.id + '-1', plugin: p.id, name: p.name, enabled: true, config: {} }) as Instance

describe('templates', () => {
  for (const t of templates) {
    it(`「${t.title}」builds a rule the editor can open and save`, () => {
      const chosen: Record<string, Instance> = {}
      for (const need of t.needs) {
        const fit = plugins.find(need.fits)
        expect(fit, `nothing fits「${need.title}」`).toBeTruthy()
        chosen[need.key] = instance(fit!)
      }
      const rule = t.build(chosen, plugins)
      const doc = fromRule(rule, new Set(['message.created']))
      expect(doc.nodes.filter((n) => n.kind === 'trigger')).toHaveLength(1)
      const known = new Set(doc.nodes.map((n) => n.id))
      for (const e of doc.edges) expect(known.has(e.from) && known.has(e.to), `${e.from} → ${e.to}`).toBe(true)
      // everything but the trigger is reached from it
      expect(new Set(doc.edges.map((e) => e.to)).size).toBe(doc.nodes.length - 1)
      // nothing is lost on the way back to a stored rule
      expect(toRule(doc, rule).graph!.nodes.map((n) => n.id)).toEqual(doc.nodes.map((n) => n.id))
    })
  }

  it("start an action from its plugin's defaults, then their own", () => {
    const chat = templates.find((t) => t.id === 'chat-ai')!
    const rule = chat.build({ chat: instance(plugins[0]), ai: instance(plugins[1]) }, plugins)
    expect(rule.graph!.nodes.find((n) => n.id === 'ai')!.params).toEqual({
      model: 'auto',
      prompt: '{{ payload.text }}',
    })
  })

  it('give a schedule a time to start from unless the connection has its own', () => {
    const t = templates.find((t) => t.id === 'schedule-ai')!
    const [when, ai, to] = [plugins[3], plugins[1], plugins[0]].map(instance)
    expect(t.build({ when, ai, to }, plugins).graph!.nodes[0].params).toHaveProperty('schedules')
    const set = { ...when, config: { schedules: '9:00 汇总' } }
    expect(t.build({ when: set, ai, to }, plugins).graph!.nodes[0].params).toBeUndefined()
  })
})

describe('ruleFromEvent', () => {
  it('starts from the same source and the same kind of event', () => {
    const rule = ruleFromEvent({ source: 'qq-1', type: 'message.created' } as HubEvent, 'QQ')
    expect(rule.name).toBe('QQ → ')
    expect(rule.graph!.nodes).toMatchObject([{ kind: 'trigger', sources: ['qq-1'], types: ['message.created'] }])
  })
})
