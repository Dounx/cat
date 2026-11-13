import { describe, expect, it } from 'vitest'
import {
  ELSE,
  JUDGE,
  NODE_W,
  blankNode,
  choicesOf,
  conditionOf,
  edgeKey,
  freshId,
  fromRule,
  layout,
  outcomes,
  toRule,
  typesOf,
  upstreamActions,
  wouldLoop,
  type Doc,
} from './graph'
import type { GraphEdge, GraphNode, Rule } from './types'

const rule = (more: Partial<Rule> = {}): Rule => ({
  id: 'r1',
  name: '规则',
  enabled: true,
  sources: [],
  types: [],
  condition: '',
  steps: [],
  ...more,
})
const declared = new Set(['message.created'])
const ids = (doc: Doc) => doc.nodes.map((n) => n.id)
const wires = (edges: GraphEdge[]) => edges.map(edgeKey)
const node = (nodes: GraphNode[], id: string) => nodes.find((n) => n.id === id)!
// A document built by hand: nodes by kind, wires as "from>to" or "from>to:branch".
function sketch(kinds: Record<string, Doc['nodes'][number]['kind']>, ...edges: string[]): Doc {
  return {
    name: '',
    enabled: true,
    nodes: Object.entries(kinds).map(([id, kind]) => blankNode(kind, id)),
    edges: edges.map((e) => {
      const [path, branch] = e.split(':')
      const [from, to] = path.split('>')
      return { from, to, ...(branch ? { branch } : {}) }
    }),
  }
}

describe('fromRule', () => {
  it('turns a rule from before graphs into a chain', () => {
    const doc = fromRule(
      rule({
        sources: ['qq'],
        types: ['message.created', 'custom.*'],
        condition: 'payload.text contains "hi"',
        steps: [
          { id: 'ai', instance: 'claude', action: 'chat', params: { prompt: 'x' } },
          { id: '', instance: '$source', action: 'reply', params: {}, continue_on_error: true },
        ],
      }),
      declared,
    )
    expect(ids(doc)).toEqual(['t1', 'c1', 'ai', 's2'])
    expect(wires(doc.edges)).toEqual(['t1|c1|', 'c1|ai|', 'ai|s2|'])
    const [t, c, , reply] = doc.nodes
    // a type the sources declare is ticked; anything else is typed
    expect(t).toMatchObject({ sources: ['qq'], pickedTypes: ['message.created'], otherTypes: 'custom.*' })
    expect(c.builder.mode).toBe('expr')
    expect(conditionOf(c)).toBe('payload.text contains "hi"')
    expect(reply).toMatchObject({ instance: '$source', action: 'reply', continue_on_error: true })
    // never placed by hand, so it is laid out: one column per step
    expect(doc.nodes.map((n) => n.x)).toEqual([0, 1, 2, 3].map((c) => c * (NODE_W + 88)))
  })

  it('gives a graph without a trigger node one for the sources of the rule', () => {
    const doc = fromRule(
      rule({
        sources: ['qq'],
        graph: {
          nodes: [{ id: 't1', kind: 'action', instance: 'http', action: 'request' }],
          edges: [{ from: 'trigger', to: 't1' }],
        },
      }),
      declared,
    )
    expect(ids(doc)).toEqual(['t1_', 't1'])
    expect(doc.nodes[0]).toMatchObject({ kind: 'trigger', sources: ['qq'] })
    expect(wires(doc.edges)).toEqual(['t1_|t1|'])
  })

  it('keeps where the nodes were put', () => {
    const doc = fromRule(
      rule({
        graph: {
          nodes: [
            { id: 't1', kind: 'trigger', instance: '', action: '', meta: { x: 40, y: 50 } },
            { id: 's1', kind: 'action', instance: 'http', action: 'request', meta: { x: 400, y: -30 } },
          ],
          edges: [{ from: 't1', to: 's1' }],
        },
      }),
      declared,
    )
    expect(doc.nodes.map((n) => [n.x, n.y])).toEqual([
      [40, 50],
      [400, -30],
    ])
  })

  it("makes a rule's own throttle the rate limit of its triggers", () => {
    const doc = fromRule(rule({ throttle_seconds: 30, throttle_key: 'actor.id' }), declared)
    expect(doc.nodes[0]).toMatchObject({ kind: 'trigger', limit: 30, limitKey: 'actor.id' })
  })

  it('reads a condition put to a model', () => {
    const doc = fromRule(
      rule({
        graph: {
          nodes: [
            { id: 't1', kind: 'trigger', instance: '', action: '' },
            { id: 'c1', kind: 'condition', instance: 'llm', action: '', params: { prompt: '?' }, choices: ['a', 'b'] },
          ],
          edges: [{ from: 't1', to: 'c1' }],
        },
      }),
      declared,
    )
    expect(doc.nodes[1]).toMatchObject({ ask: true, instance: 'llm', action: JUDGE, choices: ['a', 'b'] })
  })
})

describe('toRule', () => {
  it('stores a rate limit as a throttle every wire of the trigger goes through, and reads it back', () => {
    const doc = sketch({ t1: 'trigger', s1: 'action', s2: 'action' }, 't1>s1', 't1>s2')
    Object.assign(doc.nodes[0], { limit: 60, limitKey: ' actor.id ' })
    const out = toRule(doc, rule())
    const nodes = out.graph!.nodes
    expect(node(nodes, 't1_limit')).toMatchObject({
      kind: 'throttle',
      throttle_seconds: 60,
      throttle_key: 'actor.id',
      meta: { of: 't1' },
    })
    expect(wires(out.graph!.edges).sort()).toEqual(['t1_limit|s1|', 't1_limit|s2|', 't1|t1_limit|'])

    const back = fromRule(out, declared)
    expect(ids(back)).toEqual(['t1', 's1', 's2'])
    expect(wires(back.edges)).toEqual(['t1|s1|', 't1|s2|'])
    expect(back.nodes[0]).toMatchObject({ limit: 60, limitKey: 'actor.id' })
  })

  it('drops a wire left on a way out its condition no longer has', () => {
    const doc = sketch(
      { t1: 'trigger', c1: 'condition', s1: 'action', s2: 'action' },
      't1>c1',
      'c1>s1:gone',
      'c1>s2:else',
    )
    expect(wires(toRule(doc, rule()).graph!.edges)).toEqual(['t1|c1|', 'c1|s2|else'])
  })

  it('compiles the rows of a condition, and keeps them to open again', () => {
    const doc = sketch({ t1: 'trigger', c1: 'condition' }, 't1>c1')
    doc.nodes[1].builder.rows.push({ field: 'type', op: 'eq', value: 'x' })
    const c = node(toRule(doc, rule()).graph!.nodes, 'c1')
    expect(c.condition).toBe('type == "x"')
    expect(c.meta!.builder.rows).toHaveLength(1)
    expect(c.instance).toBe('')
  })

  it('stores a condition put to a model without an expression', () => {
    const doc = sketch({ t1: 'trigger', c1: 'condition' }, 't1>c1')
    Object.assign(doc.nodes[1], { ask: true, instance: 'llm', expr: 'left over', params: { prompt: '?' } })
    expect(node(toRule(doc, rule()).graph!.nodes, 'c1')).toMatchObject({
      instance: 'llm',
      action: JUDGE,
      condition: '',
      params: { prompt: '?' },
    })
  })

  it("keeps a trigger's parameters only for sources that take any", () => {
    const doc = sketch({ t1: 'trigger' })
    doc.nodes[0].params = { feeds: 'https://example.com/feed' }
    expect(node(toRule(doc, rule()).graph!.nodes, 't1').params).toEqual({ feeds: 'https://example.com/feed' })
    expect(node(toRule(doc, rule(), () => false).graph!.nodes, 't1').params).toBeUndefined()
  })

  it('says for the rule as a whole what its triggers take in between them', () => {
    const doc = sketch({ t1: 'trigger', t2: 'trigger' })
    Object.assign(doc.nodes[0], { sources: ['qq'], pickedTypes: ['a'] })
    Object.assign(doc.nodes[1], { sources: ['tg', 'qq'], otherTypes: 'b.*, c' })
    expect(toRule(doc, rule())).toMatchObject({ sources: ['qq', 'tg'], types: ['a', 'b.*', 'c'] })
    // one trigger that takes anything, and the rule takes anything
    doc.nodes[1].sources = []
    expect(toRule(doc, rule()).sources).toEqual([])
  })

  it('leaves the rule as before graphs empty, and what else was kept with it alone', () => {
    const out = toRule(sketch({ t1: 'trigger' }), rule({ condition: 'x', meta: { builder: {}, trigger: {}, note: 1 } }))
    expect(out).toMatchObject({ id: 'r1', condition: '', steps: [], throttle_seconds: 0, meta: { note: 1 } })
    expect(out.meta).toEqual({ note: 1 })
  })
})

describe('the ways out of a node', () => {
  it('is one for anything but a condition', () => {
    expect(outcomes(blankNode('action', 's1'))).toEqual([{ branch: '' }])
  })

  it('is yes or no for a condition without choices', () => {
    expect(outcomes(blankNode('condition', 'c1')).map((o) => o.branch)).toEqual(['', ELSE])
  })

  it('is each choice once, and the rest', () => {
    const c = blankNode('condition', 'c1')
    c.choices = [' 急 ', '', '急', ELSE, '不急']
    expect(choicesOf(c)).toEqual(['急', '不急'])
    expect(outcomes(c).map((o) => o.branch)).toEqual(['急', '不急', ELSE])
  })
})

describe('typesOf', () => {
  it('reads the typed ones split on commas of either kind and blanks', () => {
    const t = blankNode('trigger', 't1')
    Object.assign(t, { pickedTypes: ['a'], otherTypes: 'b.*，c  d' })
    expect(typesOf(t)).toEqual(['a', 'b.*', 'c', 'd'])
  })
})

describe('freshId', () => {
  it('counts on within a kind, past ids that are taken', () => {
    const doc = sketch({ t1: 'trigger', s1: 'action', s3: 'action' })
    expect(freshId(doc, 'action')).toBe('s4')
    expect(freshId(doc, 'condition')).toBe('c1')
    expect(freshId(doc, 'throttle')).toBe('r1')
    expect(freshId(sketch({ s2: 'action' }), 'action')).toBe('s3')
  })
})

describe('wouldLoop', () => {
  const doc = sketch({ t1: 'trigger', a: 'action', b: 'action', c: 'action' }, 't1>a', 'a>b', 'b>c')

  it('finds a wire back to an earlier node', () => {
    expect(wouldLoop(doc, 'c', 'a')).toBe(true)
    expect(wouldLoop(doc, 'b', 'b')).toBe(true)
  })

  it('allows a second wire forward', () => {
    expect(wouldLoop(doc, 'a', 'c')).toBe(false)
  })
})

describe('upstreamActions', () => {
  it('lists what leaves an output before a node, nearest last', () => {
    const doc = sketch(
      { t1: 'trigger', a: 'action', c1: 'condition', c2: 'condition', b: 'action', side: 'action' },
      't1>a',
      'a>c1',
      'c1>c2',
      'c2>b',
      't1>side',
    )
    doc.nodes.find((n) => n.id === 'c2')!.ask = true
    expect(upstreamActions(doc, 'b').map((n) => n.id)).toEqual(['a', 'c2'])
    expect(upstreamActions(doc, 'a')).toEqual([])
  })
})

describe('layout', () => {
  it('puts each node one column right of its furthest predecessor', () => {
    const doc = sketch({ t1: 'trigger', a: 'action', b: 'action', c: 'action' }, 't1>a', 'a>b', 't1>c', 'b>c')
    layout(doc)
    const col = (id: string) => doc.nodes.find((n) => n.id === id)!.x / (NODE_W + 88)
    expect(['t1', 'a', 'b', 'c'].map(col)).toEqual([0, 1, 2, 3])
  })

  it('stacks a column around the middle row, the plain way out first', () => {
    const doc = sketch({ t1: 'trigger', c1: 'condition', no: 'action', yes: 'action' }, 't1>c1', 'c1>no:else', 'c1>yes')
    layout(doc)
    const y = (id: string) => doc.nodes.find((n) => n.id === id)!.y
    expect(y('yes')).toBeLessThan(y('no'))
    expect(y('yes') + y('no')).toBe(0)
    expect(y('t1')).toBe(0)
  })

  it('does not hang on a loop', () => {
    const doc = sketch({ a: 'action', b: 'action' }, 'a>b', 'b>a')
    expect(() => layout(doc)).not.toThrow()
  })
})
