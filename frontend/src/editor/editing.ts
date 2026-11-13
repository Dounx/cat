// What is done to the graph on the canvas: nodes added, wired, copied and
// removed.
import { nextTick, type Ref } from 'vue'
import { useHub } from '../hub'
import { NODE_H, NODE_W, blankNode, clipboard, edgeKey, freshId, layout, endlessLoop, type ENode } from '../graph'
import type { Point } from '../components/FlowCanvas.vue'
import type { Editor } from './context'

interface Canvas {
  center(): Point
  fit(): void
}

export function useGraphEditing(editor: Editor, canvas: Ref<Canvas | undefined>) {
  const hub = useHub()
  const { doc, sel, node, triggers, actionsFor, resetParams } = editor

  function onMove(id: string, x: number, y: number) {
    const at = doc.nodes.find((n) => n.id === id)
    if (at) Object.assign(at, { x, y })
  }

  // Nudge a new node down until it is clear of the others.
  function clear(at: Point): Point {
    const taken = (p: Point) =>
      doc.nodes.some((n) => Math.abs(n.x - p.x) < NODE_W + 16 && Math.abs(n.y - p.y) < NODE_H + 12)
    const p = { x: Math.round(at.x), y: Math.round(at.y) }
    while (taken(p)) p.y += NODE_H + 24
    return p
  }
  // Add a node, wired from `from` when given, and open it for editing.
  function addNode(kind: ENode['kind'], at?: Point, from?: { id: string; branch: string }, instance = ''): ENode {
    const c = canvas.value!.center()
    const p = clear(at || { x: c.x - NODE_W / 2, y: c.y - NODE_H / 2 })
    const n = blankNode(kind, freshId(doc, kind), p.x, p.y)
    if (kind === 'action') {
      n.instance = instance
      const list = actionsFor(n)
      n.action = (list.find((a) => a.name === 'reply') || list[0])?.name || ''
      resetParams(n)
    }
    doc.nodes.push(n)
    if (from) doc.edges.push({ from: from.id, to: n.id, ...(from.branch ? { branch: from.branch } : {}) })
    sel.value = n.id
    return n
  }
  function connect(from: string, branch: string, to: string) {
    if (from === to || doc.nodes.find((n) => n.id === to)?.kind === 'trigger') return
    if (doc.edges.some((e) => e.from === from && e.to === to && (e.branch || '') === branch)) return
    if (endlessLoop(doc, from, to))
      return hub.notify('这样连成的循环停不下来：绕回去的路上要有一个条件节点，由它决定何时不再循环', true)
    doc.edges.push({ from, to, ...(branch ? { branch } : {}) })
  }
  // Put a new node in the middle of a wire.
  function splice(key: string, kind: ENode['kind'], at: Point, instance?: string) {
    const i = doc.edges.findIndex((e) => edgeKey(e) === key)
    if (i < 0) return
    const [e] = doc.edges.splice(i, 1)
    const n = addNode(
      kind,
      { x: at.x - NODE_W / 2, y: at.y - NODE_H / 2 },
      { id: e.from, branch: e.branch || '' },
      instance,
    )
    doc.edges.push({ from: n.id, to: e.to })
  }
  function removeNode(id: string) {
    const n = doc.nodes.find((x) => x.id === id)
    if (n?.kind === 'trigger' && triggers.value.length === 1) {
      return hub.notify('至少要留一个触发节点，否则这条规则永远不会运行', true)
    }
    doc.nodes.splice(
      doc.nodes.findIndex((n) => n.id === id),
      1,
    )
    doc.edges = doc.edges.filter((e) => e.from !== id && e.to !== id)
    if (sel.value === id) sel.value = null
  }
  function removeEdge(key: string) {
    doc.edges = doc.edges.filter((e) => edgeKey(e) !== key)
    if (sel.value === key) sel.value = null
  }
  function unlink(id: string) {
    doc.edges = doc.edges.filter((e) => e.from !== id && e.to !== id)
  }
  function removeSelected() {
    if (!sel.value) return
    if (node.value) removeNode(sel.value)
    else removeEdge(sel.value)
  }
  // A copy of a node placed nearby (or at `at`), under a new id and unwired.
  function paste(src: ENode, at?: Point) {
    const p = clear(at || { x: src.x + 28, y: src.y + 28 })
    const n: ENode = { ...JSON.parse(JSON.stringify(src)), id: freshId(doc, src.kind), ...p }
    doc.nodes.push(n)
    sel.value = n.id
  }
  function copy(n: ENode) {
    clipboard.node = JSON.parse(JSON.stringify(n))
    hub.notify('已复制节点')
  }
  function tidy() {
    layout(doc)
    nextTick(() => canvas.value?.fit())
  }

  return { onMove, addNode, connect, splice, removeNode, removeEdge, unlink, removeSelected, paste, copy, tidy }
}
