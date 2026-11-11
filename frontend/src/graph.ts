// The rule editor's working copy of a rule: everything the user can change,
// held as one plain object so that undo is a matter of keeping snapshots.
import { compile, type Builder } from './condition'
import type { GraphEdge, GraphNode, Rule } from './types'

export const NODE_W = 208
export const NODE_H = 64
// the id older graphs use for the rule's one, implicit trigger
const IMPLICIT_TRIGGER = 'trigger'
export const ELSE = 'else'
// the action a condition asks of the instance that judges it
export const JUDGE = 'judge'

export interface ENode {
  id: string
  kind: 'trigger' | 'condition' | 'throttle' | 'action'
  x: number
  y: number
  // trigger
  sources: string[]
  pickedTypes: string[] // event types the sources declare, ticked in a list
  otherTypes: string // anything else, typed: globs such as "message.*"
  // a trigger's own rate limit: start the rule once per `limit` seconds for
  // each value of `limitKey` (0 = no limit)
  limit: number
  limitKey: string
  // throttle: let one event past per `seconds` for each value of `key`
  seconds: number
  key: string
  // condition
  builder: Builder
  expr: string // the expression as typed, used when the builder is switched off
  // put to `instance` (a model) with `params`, instead of being worked out
  ask: boolean
  // what the condition can come to; empty means yes or no
  choices: string[]
  // action, and a condition that is asked
  instance: string
  action: string
  params: Record<string, unknown>
  continue_on_error?: boolean
  timeout_seconds?: number
}
export interface Doc {
  name: string
  enabled: boolean
  nodes: ENode[]
  edges: GraphEdge[]
}

export const edgeKey = (e: GraphEdge) => `${e.from}|${e.to}|${e.branch || ''}`
export const conditionOf = (n: ENode) => (n.builder.mode === 'builder' ? compile(n.builder) : n.expr.trim())
export const parseTypes = (text: string) => text.split(/[,，\s]+/).filter(Boolean)
export const typesOf = (n: ENode) => [...n.pickedTypes, ...parseTypes(n.otherTypes)]

export function blankNode(kind: ENode['kind'], id: string, x = 0, y = 0): ENode {
  return {
    id,
    kind,
    x,
    y,
    sources: [],
    pickedTypes: [],
    otherTypes: '',
    limit: 0,
    limitKey: '',
    seconds: 60,
    key: '',
    builder: { mode: 'builder', join: 'and', rows: [] },
    expr: '',
    ask: false,
    choices: [],
    instance: '',
    action: '',
    params: {},
  }
}

// The choices as they are saved: trimmed, none empty, none twice.
export const choicesOf = (n: ENode) => [...new Set(n.choices.map((c) => c.trim()).filter((c) => c && c !== ELSE))]
// The ways out of a node: the branch an edge carries and what to call it.
export function outcomes(n: ENode): { branch: string; label?: string }[] {
  if (n.kind !== 'condition') return [{ branch: '' }]
  const list = choicesOf(n)
  if (!list.length)
    return [
      { branch: '', label: '是' },
      { branch: ELSE, label: '否' },
    ]
  return [...list.map((c) => ({ branch: c, label: c })), { branch: ELSE, label: '其他' }]
}

// An id no node has yet: t1… for triggers, c1… for conditions, r1… for
// throttles, s1… for actions.
export function freshId(doc: Doc, kind: ENode['kind']): string {
  const prefix = { trigger: 't', condition: 'c', throttle: 'r', action: 's' }[kind]
  let n = doc.nodes.filter((x) => x.kind === kind).length + 1
  while (doc.nodes.some((x) => x.id === prefix + n)) n++
  return prefix + n
}

// Open a stored rule for editing. A rule saved before graphs existed (one
// condition, then steps in order) becomes the equivalent chain, and a rule
// whose trigger is its own sources and types gets a trigger node for them.
export function fromRule(rule: Rule, declared: Set<string>): Doc {
  const doc: Doc = {
    name: rule.name || '',
    enabled: rule.enabled,
    nodes: [],
    edges: [],
  }
  const trigger = (id: string, sources?: string[], types?: string[], at?: { x?: number; y?: number }) => {
    const t = blankNode('trigger', id, at?.x, at?.y)
    t.sources = [...(sources || [])]
    t.pickedTypes = (types || []).filter((x) => declared.has(x))
    t.otherTypes = (types || []).filter((x) => !declared.has(x)).join(', ')
    doc.nodes.push(t)
    return t
  }
  let placed = false
  if (rule.graph) {
    const stored = rule.graph.nodes || []
    // the rule's own trigger stands first, under an id nothing else has
    let own = ''
    if (!stored.some((n) => n.kind === 'trigger')) {
      own = 't1'
      while (stored.some((n) => n.id === own)) own += '_'
      trigger(own, rule.sources, rule.types, rule.meta?.trigger)
    }
    // a trigger's rate limit is stored as a throttle node right after it
    const folded = new Map<string, GraphNode>()
    for (const n of stored) if (n.kind === 'throttle' && n.meta?.of) folded.set(n.id, n)
    for (const n of stored) {
      if (folded.has(n.id)) continue
      if (n.kind === 'trigger') {
        trigger(n.id, n.sources, n.types, n.meta)
        continue
      }
      const node = blankNode(n.kind, n.id, n.meta?.x, n.meta?.y)
      if (n.kind === 'throttle') {
        node.seconds = n.throttle_seconds || 0
        node.key = n.throttle_key || ''
      } else if (n.kind === 'condition') {
        node.expr = n.condition || ''
        node.builder = n.meta?.builder || { mode: n.condition ? 'expr' : 'builder', join: 'and', rows: [] }
        node.choices = [...(n.choices || [])]
        node.ask = !!n.instance
        if (node.ask)
          Object.assign(node, {
            instance: n.instance,
            action: n.action || JUDGE,
            params: n.params || {},
            timeout_seconds: n.timeout_seconds,
          })
      } else {
        Object.assign(node, {
          instance: n.instance,
          action: n.action,
          params: n.params || {},
          continue_on_error: n.continue_on_error,
          timeout_seconds: n.timeout_seconds,
        })
      }
      doc.nodes.push(node)
    }
    doc.edges = (rule.graph.edges || [])
      .filter((e) => !folded.has(e.to))
      .map((e) => ({
        from: folded.has(e.from) ? folded.get(e.from)!.meta!.of : own && e.from === IMPLICIT_TRIGGER ? own : e.from,
        to: e.to,
        ...(e.branch ? { branch: e.branch } : {}),
      }))
    for (const f of folded.values()) {
      const t = doc.nodes.find((n) => n.id === f.meta!.of)
      if (t) Object.assign(t, { limit: f.throttle_seconds || 0, limitKey: f.throttle_key || '' })
    }
    placed =
      stored.every((n) => folded.has(n.id) || typeof n.meta?.x === 'number') &&
      (!own || typeof rule.meta?.trigger?.x === 'number')
  } else {
    let prev = trigger('t1', rule.sources, rule.types).id
    const builder: Builder | undefined = rule.meta?.builder
    if (rule.condition || builder?.rows?.length) {
      const c = blankNode('condition', 'c1')
      c.expr = rule.condition || ''
      c.builder = builder || { mode: 'expr', join: 'and', rows: [] }
      doc.nodes.push(c)
      doc.edges.push({ from: prev, to: c.id })
      prev = c.id
    }
    for (const s of rule.steps || []) {
      const node = blankNode('action', s.id || freshId(doc, 'action'))
      Object.assign(node, {
        instance: s.instance,
        action: s.action,
        params: s.params || {},
        continue_on_error: s.continue_on_error,
        timeout_seconds: s.timeout_seconds,
      })
      doc.nodes.push(node)
      doc.edges.push({ from: prev, to: node.id })
      prev = node.id
    }
  }
  // A throttle set on the rule as a whole, as rules had before graphs, is
  // the rate limit of each of its triggers.
  if (rule.throttle_seconds && !doc.nodes.some((n) => n.kind === 'throttle' || n.limit)) {
    for (const t of doc.nodes.filter((n) => n.kind === 'trigger')) {
      t.limit = rule.throttle_seconds
      t.limitKey = rule.throttle_key || ''
    }
  }
  if (!placed) layout(doc)
  return doc
}

// The rule as the hub stores it.
export function toRule(doc: Doc, base: Rule): Rule {
  // A trigger with a rate limit is followed by a throttle node that every
  // wire leaving the trigger goes through.
  const limited = new Map<string, string>()
  for (const t of doc.nodes) {
    if (t.kind !== 'trigger' || !(Number(t.limit) > 0)) continue
    let id = t.id + '_limit'
    while (doc.nodes.some((n) => n.id === id)) id += '_'
    limited.set(t.id, id)
  }
  // a wire left on a way out its condition no longer has is not kept
  const ways = new Map(doc.nodes.map((n) => [n.id, outcomes(n).map((o) => o.branch)]))
  const edges: GraphEdge[] = doc.edges
    .filter((e) => ways.get(e.from)?.includes(e.branch || ''))
    .map((e) => (limited.has(e.from) ? { ...e, from: limited.get(e.from)! } : e))
  const limits: GraphNode[] = []
  for (const [of, id] of limited) {
    const t = doc.nodes.find((n) => n.id === of)!
    limits.push({
      id,
      kind: 'throttle',
      instance: '',
      action: '',
      throttle_seconds: Number(t.limit),
      throttle_key: t.limitKey.trim(),
      meta: { of },
    })
    edges.push({ from: of, to: id })
  }
  const nodes: GraphNode[] = doc.nodes.map((n) => {
    const at = { x: n.x, y: n.y }
    if (n.kind === 'trigger')
      return { id: n.id, kind: n.kind, instance: '', action: '', sources: n.sources, types: typesOf(n), meta: at }
    if (n.kind === 'throttle')
      return {
        id: n.id,
        kind: n.kind,
        instance: '',
        action: '',
        throttle_seconds: Number(n.seconds) || 0,
        throttle_key: n.key.trim(),
        meta: at,
      }
    if (n.kind === 'condition') {
      const choices = choicesOf(n)
      return {
        id: n.id,
        kind: n.kind,
        instance: n.ask ? n.instance : '',
        action: n.ask ? n.action || JUDGE : '',
        ...(n.ask ? { params: n.params, timeout_seconds: n.timeout_seconds || undefined } : {}),
        condition: n.ask ? '' : conditionOf(n),
        ...(choices.length ? { choices } : {}),
        meta: { ...at, builder: n.builder },
      }
    }
    return {
      id: n.id,
      kind: n.kind,
      instance: n.instance,
      action: n.action,
      params: n.params,
      continue_on_error: n.continue_on_error || undefined,
      timeout_seconds: n.timeout_seconds || undefined,
      meta: at,
    }
  })
  const meta: Record<string, any> = { ...base.meta }
  delete meta.builder
  delete meta.trigger
  // The rule's own sources and types only say, for lists and filters, what
  // the triggers take in between them: empty as soon as one takes anything.
  const triggers = doc.nodes.filter((n) => n.kind === 'trigger')
  const union = (lists: string[][]) => (lists.some((l) => !l.length) ? [] : [...new Set(lists.flat())])
  return {
    id: base.id,
    name: doc.name,
    enabled: doc.enabled,
    sources: union(triggers.map((t) => t.sources)),
    types: union(triggers.map(typesOf)),
    condition: '',
    steps: [],
    graph: { nodes: [...nodes, ...limits], edges },
    throttle_seconds: 0,
    throttle_key: '',
    meta,
  }
}

// Would an edge from → to close a loop? It would if `from` can already be
// reached from `to`.
export function wouldLoop(doc: Doc, from: string, to: string): boolean {
  const todo = [to]
  const seen = new Set<string>()
  while (todo.length) {
    const id = todo.pop()!
    if (id === from) return true
    if (seen.has(id)) continue
    seen.add(id)
    for (const e of doc.edges) if (e.from === id) todo.push(e.to)
  }
  return false
}

// The nodes that leave an output before `id` on some path, nearest last:
// actions, and conditions that were put to a model.
export function upstreamActions(doc: Doc, id: string): ENode[] {
  const seen = new Set<string>()
  const order: string[] = []
  const visit = (at: string) => {
    for (const e of doc.edges) {
      if (e.to !== at || seen.has(e.from)) continue
      seen.add(e.from)
      visit(e.from)
      order.push(e.from)
    }
  }
  visit(id)
  return order
    .map((x) => doc.nodes.find((n) => n.id === x)!)
    .filter((n) => n?.kind === 'action' || (n?.kind === 'condition' && n.ask))
}

// Lay the graph out in columns: triggers on the left, and every other node
// one column right of its furthest predecessor. Each column is stacked around
// the same middle row.
export function layout(doc: Doc) {
  const depth = new Map<string, number>()
  const depthOf = (id: string, trail: Set<string>): number => {
    if (depth.has(id)) return depth.get(id)!
    if (trail.has(id)) return 1
    trail.add(id)
    const before = doc.edges.filter((e) => e.to === id).map((e) => depthOf(e.from, trail))
    const own = doc.nodes.find((n) => n.id === id)?.kind === 'trigger' ? 0 : 1
    const d = before.length ? Math.max(...before) + 1 : own
    depth.set(id, d)
    return d
  }
  // walk from the triggers so that rows follow the order of the wires
  const order: string[] = []
  const walk = (id: string) => {
    if (order.includes(id)) return
    order.push(id)
    const out = doc.edges.filter((x) => x.from === id).sort((a, b) => (a.branch ? 1 : 0) - (b.branch ? 1 : 0))
    for (const e of out) walk(e.to)
  }
  for (const n of doc.nodes) if (n.kind === 'trigger') walk(n.id)
  for (const n of doc.nodes) walk(n.id)
  const cols: string[][] = []
  for (const id of order) (cols[depthOf(id, new Set())] ||= []).push(id)
  const gapX = 88
  const gapY = 32
  cols.forEach((ids, c) =>
    ids?.forEach((id, i) => {
      const n = doc.nodes.find((x) => x.id === id)!
      n.x = c * (NODE_W + gapX)
      n.y = Math.round((i - (ids.length - 1) / 2) * (NODE_H + gapY))
    }),
  )
}

// A node copied in one rule can be pasted into another.
export const clipboard: { node: ENode | null } = { node: null }
