<script setup lang="ts">
// A text field in which {{ expression }} placeholders show as named tags:
// 「内容」 rather than {{ payload.text }}. The value stays the plain template
// string; a placeholder nothing has a name for is left as typed.
import { onMounted, ref, watch } from 'vue'

const props = defineProps<{
  modelValue: string
  multiline?: boolean
  placeholder?: string
  // the name of what an expression refers to, if it has one
  labelOf: (expr: string) => string | undefined
}>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

const el = ref<HTMLElement>()
let shown = ''

function chip(raw: string, expr: string, label: string): HTMLElement {
  const c = document.createElement('span')
  c.className = 'var-chip'
  c.contentEditable = 'false'
  c.dataset.raw = raw
  c.title = expr
  c.textContent = label
  return c
}
// Draw a template string: text, line breaks, and a tag for each placeholder.
function draw(value: string) {
  const root = el.value!
  root.textContent = ''
  const text = (s: string) => {
    s.split('\n').forEach((line, i) => {
      if (i) root.appendChild(document.createElement('br'))
      if (line) root.appendChild(document.createTextNode(line))
    })
  }
  let at = 0
  for (const m of value.matchAll(/\{\{\s*([^{}]+?)\s*\}\}/g)) {
    const label = props.labelOf(m[1])
    if (!label) continue
    text(value.slice(at, m.index))
    root.appendChild(chip(m[0], m[1], label))
    at = m.index! + m[0].length
  }
  text(value.slice(at))
  // a line break at the very end shows only with something after it
  if (root.lastChild?.nodeName === 'BR') root.appendChild(document.createElement('br'))
  shown = value
}
// Read a template string back off drawn nodes.
function serialize(node: Node): string {
  let out = ''
  const walk = (node: Node) => {
    for (const n of Array.from(node.childNodes)) {
      if (n.nodeType === Node.TEXT_NODE) out += (n.textContent || '').replace(/\u00a0/g, ' ')
      else if (n.nodeName === 'BR') out += '\n'
      else if ((n as HTMLElement).dataset?.raw) out += (n as HTMLElement).dataset.raw
      else {
        // a block the browser made for a new line
        if (/^(DIV|P)$/.test(n.nodeName) && out && !out.endsWith('\n')) out += '\n'
        walk(n)
      }
    }
  }
  walk(node)
  return out
}
function read(): string {
  const out = serialize(el.value!)
  // the filler break after a final line break is not content
  return el.value!.lastChild?.nodeName === 'BR' && out.endsWith('\n') ? out.slice(0, -1) : out
}
function changed() {
  shown = read()
  emit('update:modelValue', shown)
}

onMounted(() => draw(props.modelValue || ''))
watch(
  () => props.modelValue,
  (v) => {
    if ((v || '') !== shown) draw(v || '')
  },
)

function onKey(e: KeyboardEvent) {
  if (e.key !== 'Enter' || e.isComposing) return
  e.preventDefault()
  if (props.multiline) put('\n')
}
function onPaste(e: ClipboardEvent) {
  e.preventDefault()
  const text = e.clipboardData?.getData('text/plain') || ''
  put(props.multiline ? text.replace(/\r\n?/g, '\n') : text.replace(/\s*[\r\n]+\s*/g, ' '))
}

// Put text where the caret is (at the end when the caret is elsewhere),
// replacing what is selected, and redraw: a placeholder in it becomes a tag.
function put(text: string) {
  const root = el.value!
  root.focus()
  const sel = window.getSelection()!
  let range = sel.rangeCount ? sel.getRangeAt(0) : null
  if (!range || !root.contains(range.commonAncestorContainer)) {
    range = document.createRange()
    range.selectNodeContents(root)
    range.collapse(false)
  }
  range.deleteContents()
  const head = document.createRange()
  head.selectNodeContents(root)
  head.setEnd(range.startContainer, range.startOffset)
  const before = serialize(head.cloneContents())
  const value = before + text + read().slice(before.length)
  draw(value)
  placeCaret(before.length + text.length)
  emit('update:modelValue', shown)
}
// Put the caret after the first `offset` characters of the value.
function placeCaret(offset: number) {
  const root = el.value!
  const range = document.createRange()
  let left = offset
  let placed = false
  for (const n of Array.from(root.childNodes)) {
    const isText = n.nodeType === Node.TEXT_NODE
    const len = isText
      ? (n.textContent || '').length
      : n.nodeName === 'BR'
        ? 1
        : ((n as HTMLElement).dataset?.raw || '').length
    if (left <= len && (isText || left === len)) {
      if (isText) range.setStart(n, left)
      else range.setStartAfter(n)
      placed = true
      break
    }
    left -= len
  }
  if (!placed) {
    range.selectNodeContents(root)
    range.collapse(false)
  }
  range.collapse(true)
  const sel = window.getSelection()!
  sel.removeAllRanges()
  sel.addRange(range)
}

const insert = (expr: string) => put(`{{ ${expr} }}`)
// Replace everything with one value.
function replace(value: string) {
  draw(value)
  emit('update:modelValue', shown)
}
defineExpose({ insert, replace })
</script>

<template>
  <div
    ref="el"
    class="input template-input"
    :class="{ multi: multiline }"
    contenteditable="true"
    role="textbox"
    :aria-multiline="multiline"
    :data-placeholder="placeholder"
    spellcheck="false"
    @input="changed"
    @keydown="onKey"
    @paste="onPaste"
  />
</template>

<style>
.template-input {
  min-height: 2.1rem;
  line-height: 1.55;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  cursor: text;
}
.template-input.multi {
  min-height: 4.6rem;
  max-height: 16rem;
  overflow-y: auto;
}
.template-input:empty::before {
  content: attr(data-placeholder);
  color: color-mix(in srgb, var(--color-mute) 60%, transparent);
  pointer-events: none;
}
.var-chip {
  display: inline-block;
  margin: 0 1px;
  padding: 0 7px;
  border-radius: 6px;
  background: var(--color-accent-soft);
  color: var(--color-accent);
  font-size: 12px;
  line-height: 1.5;
  white-space: nowrap;
  user-select: all;
  cursor: default;
}
</style>
