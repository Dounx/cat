<script setup lang="ts">
// The canvas a rule's graph is drawn and wired on. Nodes can be dragged about,
// a wire is pulled from a node's output port onto another node, and the view
// pans and zooms. The canvas owns only the view; every change to the graph is
// reported to the parent, which holds it.
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch, type Component } from 'vue'
import { Maximize, Minus, Plus } from 'lucide-vue-next'
import { ELSE, NODE_H as H, NODE_W as W, edgeKey } from '../graph'
import type { GraphEdge } from '../types'

export interface CanvasNode {
  id: string
  kind: 'trigger' | 'condition' | 'action'
  x: number
  y: number
  icon: Component
  label: string
  title: string
  sub?: string
  // the ways out of a condition, top to bottom; any other node has just one
  outs?: { branch: string; label?: string }[]
  bad?: boolean
  // how the node fared in a trial run
  state?: 'ok' | 'bad'
}
export interface Point {
  x: number
  y: number
}

const props = defineProps<{
  nodes: CanvasNode[]
  edges: GraphEdge[]
  selected: string | null // a node id or an edge key
  padRight?: number // width covered by a panel, kept clear when fitting
}>()
const emit = defineEmits<{
  select: [id: string | null]
  move: [id: string, x: number, y: number]
  connect: [from: string, branch: string, to: string]
  // a wire let go over empty canvas, or an output port clicked: offer what to
  // add there; `at` is where the new node's top left corner would go
  spawn: [from: string, branch: string, at: Point, e: PointerEvent]
  menu: [target: { node?: string; edge?: string } | null, at: Point, e: MouseEvent]
}>()

const root = ref<HTMLElement>()
const view = reactive({ x: 0, y: 0, k: 1 })
const MIN = 0.3
const MAX = 2

// Output ports sit on a node's right edge; a condition has one for each
// outcome, and grows taller to fit more than a few.
const outsOf = (n: CanvasNode) => (n.outs?.length ? n.outs : [{ branch: '' }])
const heightOf = (n: CanvasNode) => Math.max(H, outsOf(n).length * 20 + 16)
function ports(n: CanvasNode): { branch: string; y: number; label?: string }[] {
  const outs = outsOf(n)
  const gap = outs.length === 2 ? 24 : 20
  return outs.map((o, i) => ({ ...o, y: heightOf(n) / 2 + (i - (outs.length - 1) / 2) * gap }))
}
const byId = computed(() => new Map(props.nodes.map((n) => [n.id, n])))

function curve(a: Point, b: Point): string {
  const bend = Math.max(36, Math.abs(b.x - a.x) / 2)
  return `M ${a.x} ${a.y} C ${a.x + bend} ${a.y}, ${b.x - bend} ${b.y}, ${b.x} ${b.y}`
}
// A wire to a node that is not to the right, as one that loops back is, goes
// round underneath (at `under`) rather than across what lies between.
function around(a: Point, b: Point, under: number): string {
  const r = 44
  return (
    `M ${a.x} ${a.y} C ${a.x + r} ${a.y}, ${a.x + r} ${under}, ${a.x} ${under} ` +
    `L ${b.x} ${under} C ${b.x - r} ${under}, ${b.x - r} ${b.y}, ${b.x} ${b.y}`
  )
}
const wires = computed(() =>
  props.edges.flatMap((e) => {
    const from = byId.value.get(e.from)
    const to = byId.value.get(e.to)
    if (!from || !to) return []
    const port = ports(from).find((p) => p.branch === (e.branch || '')) || ports(from)[0]
    const a = { x: from.x + W, y: from.y + port.y }
    const b = { x: to.x - 2, y: to.y + heightOf(to) / 2 }
    const under = Math.max(from.y + heightOf(from), to.y + heightOf(to)) + 28
    return [{ key: edgeKey(e), d: b.x < a.x + 24 ? around(a, b, under) : curve(a, b), no: e.branch === ELSE }]
  }),
)

// client → graph coordinates
function point(e: { clientX: number; clientY: number }): Point {
  const r = root.value!.getBoundingClientRect()
  return { x: (e.clientX - r.left - view.x) / view.k, y: (e.clientY - r.top - view.y) / view.k }
}

// ---- pointer: drag a node, pull a wire, or pan ----
const pulling = ref<null | { from: string; branch: string; a: Point; b: Point }>(null)
const dragging = ref('')
const busy = ref(false)
let gesture: null | { move: (e: PointerEvent) => void; up: (e: PointerEvent, moved: boolean) => void } = null
let start = { x: 0, y: 0 }
let moved = false

function begin(e: PointerEvent, g: NonNullable<typeof gesture>) {
  if (e.button !== 0) return
  gesture = g
  busy.value = true
  start = { x: e.clientX, y: e.clientY }
  moved = false
  window.addEventListener('pointermove', onMove)
  window.addEventListener('pointerup', onUp)
}
function onMove(e: PointerEvent) {
  if (!moved && Math.hypot(e.clientX - start.x, e.clientY - start.y) < 4) return
  moved = true
  gesture?.move(e)
}
function onUp(e: PointerEvent) {
  window.removeEventListener('pointermove', onMove)
  window.removeEventListener('pointerup', onUp)
  const g = gesture
  gesture = null
  busy.value = false
  g?.up(e, moved)
}
onBeforeUnmount(() => {
  window.removeEventListener('pointermove', onMove)
  window.removeEventListener('pointerup', onUp)
})

function downNode(e: PointerEvent, n: CanvasNode) {
  const grab = point(e)
  const off = { x: grab.x - n.x, y: grab.y - n.y }
  begin(e, {
    move(ev) {
      dragging.value = n.id
      const p = point(ev)
      emit('move', n.id, Math.round(p.x - off.x), Math.round(p.y - off.y))
    },
    up(_, wasMoved) {
      dragging.value = ''
      if (!wasMoved) emit('select', n.id)
    },
  })
}
function downPort(e: PointerEvent, n: CanvasNode, port: { branch: string; y: number }) {
  const a = { x: n.x + W, y: n.y + port.y }
  begin(e, {
    move(ev) {
      pulling.value = { from: n.id, branch: port.branch, a, b: point(ev) }
    },
    up(ev, wasMoved) {
      pulling.value = null
      // a plain click on the port asks for a next node just to its right
      if (!wasMoved) return emit('spawn', n.id, port.branch, { x: n.x + W + 88, y: n.y + port.y - H / 2 }, ev)
      const over = document.elementFromPoint(ev.clientX, ev.clientY)?.closest<HTMLElement>('[data-node]')
      const to = over?.dataset.node
      const p = point(ev)
      if (to && to !== n.id) emit('connect', n.id, port.branch, to)
      else if (!over && !document.elementFromPoint(ev.clientX, ev.clientY)?.closest('.node'))
        emit('spawn', n.id, port.branch, { x: p.x, y: p.y - H / 2 }, ev)
    },
  })
}
function downBlank(e: PointerEvent) {
  const from = { x: view.x, y: view.y }
  begin(e, {
    move(ev) {
      view.x = from.x + ev.clientX - start.x
      view.y = from.y + ev.clientY - start.y
    },
    up(_, wasMoved) {
      if (!wasMoved) emit('select', null)
    },
  })
}

// ---- view ----
function zoomAt(k: number, cx: number, cy: number) {
  k = Math.min(MAX, Math.max(MIN, k))
  view.x = cx - ((cx - view.x) / view.k) * k
  view.y = cy - ((cy - view.y) / view.k) * k
  view.k = k
}
// The wheel pans; with Ctrl or ⌘ (which is also how a pinch arrives) it zooms.
function onWheel(e: WheelEvent) {
  const r = root.value!.getBoundingClientRect()
  if (e.ctrlKey || e.metaKey) zoomAt(view.k * Math.exp(-e.deltaY * 0.01), e.clientX - r.left, e.clientY - r.top)
  else {
    view.x -= e.deltaX
    view.y -= e.deltaY
  }
}
function zoom(factor: number) {
  const el = root.value!
  zoomAt(view.k * factor, (el.clientWidth - (props.padRight || 0)) / 2, el.clientHeight / 2)
}
// Bring the whole graph into view, never enlarged past its natural size.
function fit() {
  const el = root.value
  if (!el || !props.nodes.length) return
  const pad = 56
  const w = Math.max(200, el.clientWidth - (props.padRight || 0))
  const h = el.clientHeight
  const x0 = Math.min(...props.nodes.map((n) => n.x))
  const y0 = Math.min(...props.nodes.map((n) => n.y))
  const x1 = Math.max(...props.nodes.map((n) => n.x + W))
  const y1 = Math.max(...props.nodes.map((n) => n.y + heightOf(n)))
  const k = Math.min(1, Math.max(MIN, Math.min((w - pad * 2) / (x1 - x0), (h - pad * 2) / (y1 - y0))))
  view.k = k
  view.x = (w - (x1 - x0) * k) / 2 - x0 * k
  view.y = (h - (y1 - y0) * k) / 2 - y0 * k
}
// The middle of what is on screen, in graph coordinates.
function center(): Point {
  const el = root.value!
  return {
    x: ((el.clientWidth - (props.padRight || 0)) / 2 - view.x) / view.k,
    y: (el.clientHeight / 2 - view.y) / view.k,
  }
}
// Slide the view so the picked node is not left under the panel or off screen.
watch(
  () => [props.selected, props.padRight],
  () => {
    const n = props.selected && byId.value.get(props.selected)
    const el = root.value
    if (!n || !el) return
    const gap = 24
    const right = el.clientWidth - (props.padRight || 0) - gap
    const x0 = n.x * view.k + view.x
    const y0 = n.y * view.k + view.y
    if (x0 + W * view.k > right) view.x -= x0 + W * view.k - right
    else if (x0 < gap) view.x += gap - x0
    const h = heightOf(n) * view.k
    if (y0 + h > el.clientHeight - gap) view.y -= y0 + h - (el.clientHeight - gap)
    else if (y0 < gap) view.y += gap - y0
  },
  { flush: 'post' },
)
onMounted(fit)
defineExpose({ fit, zoom, center })
</script>

<template>
  <div
    ref="root"
    class="canvas relative overflow-hidden"
    :class="{ panning: busy }"
    :style="{
      backgroundPosition: `${view.x}px ${view.y}px`,
      backgroundSize: `${18 * view.k}px ${18 * view.k}px`,
    }"
    @pointerdown="downBlank"
    @wheel.prevent="onWheel"
    @contextmenu.prevent="emit('menu', null, point($event), $event)"
  >
    <div
      class="absolute top-0 left-0 origin-top-left"
      :style="{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.k})` }"
    >
      <svg class="pointer-events-none absolute overflow-visible" width="1" height="1">
        <defs>
          <marker id="wire-tip" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
            <path d="M 1 1 L 7 4 L 1 7" fill="none" stroke="context-stroke" stroke-width="1.5" />
          </marker>
        </defs>
        <g v-for="w in wires" :key="w.key" class="wire" :class="{ on: selected === w.key, no: w.no }">
          <path class="line" :d="w.d" marker-end="url(#wire-tip)" />
          <path
            class="hit"
            :d="w.d"
            @pointerdown.stop
            @click.stop="emit('select', w.key)"
            @contextmenu.prevent.stop="emit('menu', { edge: w.key }, point($event), $event)"
          />
        </g>
        <path v-if="pulling" class="pull" :d="curve(pulling.a, pulling.b)" />
      </svg>

      <div
        v-for="n in nodes"
        :key="n.id"
        class="node"
        :class="{ on: selected === n.id, bad: n.bad, drag: dragging === n.id, named: !!n.outs?.[0]?.branch }"
        :data-node="n.kind === 'trigger' ? undefined : n.id"
        :style="{ left: n.x + 'px', top: n.y + 'px', width: W + 'px', height: heightOf(n) + 'px' }"
        :title="n.title"
        @pointerdown.stop="downNode($event, n)"
        @contextmenu.prevent.stop="emit('menu', { node: n.id }, point($event), $event)"
      >
        <span v-if="n.kind !== 'trigger'" class="inlet" />
        <span v-if="n.state" class="ran" :class="n.state" />
        <span class="glyph" :class="n.kind"><component :is="n.icon" :size="15" /></span>
        <span class="min-w-0 flex-1">
          <span class="block truncate text-[10.5px] leading-tight text-mute">{{ n.label }}</span>
          <span class="block truncate text-[13px] leading-snug font-medium">{{ n.title }}</span>
          <span v-if="n.sub" class="block truncate text-[11px] leading-tight text-mute">{{ n.sub }}</span>
        </span>
        <template v-for="p in ports(n)" :key="p.branch">
          <span
            v-if="p.label"
            class="outcome"
            :class="{ no: p.branch === ELSE }"
            :style="{ top: p.y + 'px' }"
            :title="p.label"
            >{{ p.label }}</span
          >
          <button
            type="button"
            class="port"
            :class="{ no: p.branch === ELSE }"
            :style="{ top: p.y + 'px' }"
            title="拖到另一个节点上连线，或点一下添加下一步"
            aria-label="添加下一步"
            @pointerdown.stop="downPort($event, n, p)"
          >
            <Plus :size="9" :stroke-width="3" />
          </button>
        </template>
      </div>
    </div>

    <div class="pop absolute bottom-3 left-3 flex items-center gap-0.5 !rounded-lg p-0.5" @pointerdown.stop>
      <button class="btn btn-ghost px-1.5" title="缩小" aria-label="缩小" @click="zoom(1 / 1.2)">
        <Minus :size="14" />
      </button>
      <button class="btn btn-ghost w-12 px-0 text-xs tabular-nums" title="恢复 100%" @click="zoom(1 / view.k)">
        {{ Math.round(view.k * 100) }}%
      </button>
      <button class="btn btn-ghost px-1.5" title="放大" aria-label="放大" @click="zoom(1.2)">
        <Plus :size="14" />
      </button>
      <button class="btn btn-ghost px-1.5" title="适应窗口" aria-label="适应窗口" @click="fit">
        <Maximize :size="14" />
      </button>
    </div>
  </div>
</template>

<style scoped>
.canvas {
  background-color: var(--color-bg);
  background-image: radial-gradient(color-mix(in srgb, var(--color-faint) 55%, transparent) 1px, transparent 1px);
  cursor: grab;
  touch-action: none;
  user-select: none;
}
.canvas.panning {
  cursor: grabbing;
}

.wire .line {
  fill: none;
  stroke: var(--color-faint);
  stroke-width: 1.6;
  transition: stroke 0.12s;
}
.wire.no .line {
  stroke-dasharray: 5 4;
}
.wire .hit {
  fill: none;
  stroke: transparent;
  stroke-width: 14;
  pointer-events: stroke;
  cursor: pointer;
}
.wire:hover .line {
  stroke: var(--color-mute);
}
.wire.on .line {
  stroke: var(--color-accent);
  stroke-width: 2;
}
.pull {
  fill: none;
  stroke: var(--color-accent);
  stroke-width: 1.6;
  stroke-dasharray: 4 4;
}

.node {
  position: absolute;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 0 14px 0 12px;
  border: 1px solid var(--color-line);
  border-radius: 12px;
  background: var(--color-surface);
  box-shadow: var(--card-edge);
  cursor: pointer;
  transition:
    border-color 0.15s,
    box-shadow 0.2s var(--ease-snap);
}
.node:hover {
  border-color: color-mix(in srgb, var(--color-accent) 55%, var(--color-line));
}
.node.on {
  border-color: var(--color-accent);
  box-shadow: 0 0 0 3px color-mix(in srgb, var(--color-accent) 20%, transparent);
}
.node.drag {
  z-index: 1;
  box-shadow: 0 10px 26px rgba(0, 0, 0, 0.22);
  cursor: grabbing;
}
.glyph {
  display: flex;
  width: 28px;
  height: 28px;
  flex: none;
  align-items: center;
  justify-content: center;
  border-radius: 8px;
  background: var(--color-accent-soft);
  color: var(--color-accent);
}
.glyph.trigger {
  background: color-mix(in srgb, var(--color-warn) 16%, transparent);
  color: var(--color-warn);
}
.glyph.condition {
  background: color-mix(in srgb, var(--color-ok) 16%, transparent);
  color: var(--color-ok);
}
.ran {
  position: absolute;
  top: -4px;
  right: -4px;
  width: 11px;
  height: 11px;
  border: 2px solid var(--color-surface);
  border-radius: 50%;
  background: var(--color-ok);
}
.ran.bad {
  background: var(--color-bad);
}
.node.bad .glyph {
  background: color-mix(in srgb, var(--color-bad) 14%, transparent);
  color: var(--color-bad);
}

.inlet {
  position: absolute;
  top: 50%;
  left: -5px;
  width: 9px;
  height: 9px;
  border: 1.5px solid var(--color-faint);
  border-radius: 50%;
  background: var(--color-surface);
  transform: translateY(-50%);
}
.port {
  position: absolute;
  right: -8px;
  display: flex;
  width: 16px;
  height: 16px;
  align-items: center;
  justify-content: center;
  border: 1.5px solid var(--color-faint);
  border-radius: 50%;
  background: var(--color-surface);
  color: var(--color-mute);
  cursor: crosshair;
  transform: translateY(-50%);
  transition:
    transform 0.14s var(--ease-snap),
    border-color 0.12s,
    color 0.12s,
    background-color 0.12s;
}
.port:hover {
  border-color: var(--color-accent);
  background: var(--color-accent);
  color: var(--color-accent-fg);
  transform: translateY(-50%) scale(1.25);
}
.outcome {
  position: absolute;
  right: 12px;
  font-size: 10.5px;
  line-height: 1;
  color: var(--color-ok);
  transform: translateY(-50%);
  pointer-events: none;
}
/* A condition with named choices keeps its right side for their names. */
.node.named {
  padding-right: 76px;
}
.node.named .outcome {
  max-width: 60px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.outcome.no {
  color: var(--color-mute);
}
</style>
