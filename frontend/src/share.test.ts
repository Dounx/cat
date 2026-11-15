import { describe, expect, it } from 'vitest'
import { exportRule, fileName, importRule, needsOf, parseShared, suggest } from './share'
import type { Instance, Plugin, Rule } from './types'

const plugin = (id: string, more: Partial<Plugin> = {}) =>
  ({ id, name: id, capabilities: [], ...more }) as unknown as Plugin
const inst = (id: string, p: string, name = id) => ({ id, plugin: p, name, enabled: true, config: {} }) as Instance
const plugins = [
  plugin('telegram'),
  plugin('claude'),
  plugin('http', { singleton: true }),
  plugin('rss', { default_instance: true }),
]
const rule: Rule = {
  id: 'r1',
  name: '转给 AI / 再回复',
  enabled: false,
  steps: [],
  graph: {
    nodes: [
      { id: 't1', kind: 'trigger', instance: '', action: '', sources: ['telegram-aa', 'rss'] },
      { id: 'ai', kind: 'action', instance: 'claude-bb', action: 'chat', params: { prompt: '{{ payload.text }}' } },
      { id: 'reply', kind: 'action', instance: '$source', action: 'reply' },
      { id: 'call', kind: 'action', instance: 'http', action: 'request', meta: { x: 1, y: 2 } },
    ],
    edges: [
      { from: 't1', to: 'ai' },
      { from: 'ai', to: 'reply' },
    ],
  },
}
const here = [
  inst('telegram-aa', 'telegram', '我的 TG'),
  inst('claude-bb', 'claude'),
  inst('http', 'http'),
  inst('rss', 'rss'),
]

describe('sharing a rule', () => {
  it('says what each connection it names is, and leaves $source alone', () => {
    const shared = exportRule(rule, here)
    expect(shared.uses).toEqual({
      'telegram-aa': { plugin: 'telegram', name: '我的 TG' },
      'claude-bb': { plugin: 'claude', name: 'claude-bb' },
      http: { plugin: 'http', name: 'http' },
      rss: { plugin: 'rss', name: 'rss' },
    })
    expect(parseShared(JSON.stringify(shared))).toEqual(shared)
  })
  it('refuses what is not a shared rule', () => {
    expect(() => parseShared('nope')).toThrow('JSON')
    expect(() => parseShared('{"name":"x"}')).toThrow('不是 Cat 导出的规则')
    expect(() => parseShared(JSON.stringify({ ...exportRule(rule, here), version: 2 }))).toThrow('更新版本')
    expect(() => exportRule({ ...rule, graph: undefined }, here)).toThrow()
  })
  it('comes back as it was on the hub it left', () => {
    const shared = exportRule(rule, here)
    const needs = needsOf(shared, here, plugins)
    // what comes ready with its plugin is not asked about
    expect(needs.map((n) => n.id)).toEqual(['telegram-aa', 'claude-bb'])
    const back = importRule(shared, suggest(needs))
    expect(back.graph).toEqual(rule.graph)
    expect(back).toMatchObject({ id: '', name: rule.name, enabled: true })
  })
  it('takes the instances of another hub in place of the ones it named', () => {
    const there = [
      inst('telegram-zz', 'telegram'),
      inst('claude-1', 'claude'),
      inst('claude-2', 'claude'),
      inst('http', 'http'),
    ]
    const shared = exportRule(rule, here)
    const needs = needsOf(shared, there, plugins)
    // rss is not there under its own id, so it is asked about; with two that
    // fit, none is suggested
    expect(needs.map((n) => [n.id, n.fits.length])).toEqual([
      ['telegram-aa', 1],
      ['rss', 0],
      ['claude-bb', 2],
    ])
    const chosen = suggest(needs)
    expect(chosen).toEqual({ 'telegram-aa': 'telegram-zz', 'claude-bb': '', rss: '' })
    const made = importRule(shared, { ...chosen, 'claude-bb': 'claude-2' })
    expect(made.graph!.nodes[0].sources).toEqual(['telegram-zz', 'rss'])
    expect(made.graph!.nodes[1].instance).toBe('claude-2')
    expect(made.graph!.nodes[2].instance).toBe('$source')
  })
  it('names the file after the rule', () => {
    expect(fileName(rule.name)).toBe('转给-AI-再回复.cat-rule.json')
    expect(fileName(' / ')).toBe('rule.cat-rule.json')
  })
})
