<script setup lang="ts">
// The panel of a condition node: how it is worked out (rows, an expression,
// or a model that is asked) and what it can come to.
import { computed, ref, watch } from 'vue'
import { BrainCircuit, Filter, Plus, SquareFunction, Trash2 } from 'lucide-vue-next'
import { useHub, clock, summarize } from '../../hub'
import { post } from '../../api'
import { operators } from '../../condition'
import { useEditor } from '../../editor/context'
import { EXPR, ROWS } from '../../editor/picking'
import { ELSE, JUDGE, conditionOf, onLoop, type ENode } from '../../graph'
import { eventTitle, fieldLabel } from '../../labels'
import type { Schema } from '../../types'
import SchemaForm from '../SchemaForm.vue'
import PanelHead from './PanelHead.vue'
import TrialResult from './TrialResult.vue'

const props = defineProps<{ node: ENode }>()
defineEmits<{ remove: [] }>()
const hub = useHub()
const editor = useEditor()
const { doc, scopedEvents, testEventId, typedRows, specOf, resetParams, variablesFor, trialStep, trialSteps } = editor
// A condition on a loop can also go by how many times the rule has gone back.
const fieldHints = computed(() =>
  onLoop(doc, props.node.id) ? [...editor.fieldHints.value, 'loop'] : editor.fieldHints.value,
)

// ---- rows ----
// A row's field is chosen by name; one not on the list is typed as it is
// written in the data.
const OTHER = '__other__'
const typedField = (row: { field: string }) => typedRows.has(row) || !fieldHints.value.includes(row.field)
function chooseField(row: { field: string }, value: string) {
  if (value === OTHER) {
    typedRows.add(row)
    row.field = ''
  } else row.field = value
}
// Switching to the expression editor keeps what the builder had compiled.
function showExpression(n: ENode) {
  if (n.builder.mode !== 'expr') n.expr = conditionOf(n)
  n.builder.mode = 'expr'
  n.ask = false
}
// The rows of the builder only ever come to yes or no.
function showBuilder(n: ENode) {
  if (n.choices.length) setChoices(n, false)
  n.builder.mode = 'builder'
  n.ask = false
}

// ---- put to a model ----
// The question's form, without the choices: those are the node's own.
function judgeSchema(n: ENode): Schema | undefined {
  const params = specOf(n)?.params
  if (!params) return undefined
  const { choices: _, ...properties } = params.properties || {}
  return { ...params, properties, 'x-order': params['x-order']?.filter((k) => k !== 'choices') }
}
// Whatever offers a "judge" action can be asked: it is given the question
// and the choices, and picks one.
function showAsk(n: ENode, instance: string) {
  Object.assign(n, { ask: true, action: JUDGE, instance })
  if (!n.params?.prompt) resetParams(n)
}

// ---- how it is worked out ----
// Picked from the same list a new condition is, and changed there too.
const way = computed(() => {
  const n = props.node
  if (n.ask) return { icon: BrainCircuit, name: `模型判断 · ${hub.instanceName(n.instance) || '未选择'}` }
  return n.builder.mode === 'expr' ? { icon: SquareFunction, name: '表达式' } : { icon: Filter, name: '按字段判断' }
})
function rework() {
  editor.pickCondition('换一种方式来判断', (pick) => {
    const n = props.node
    if (pick === ROWS) showBuilder(n)
    else if (pick === EXPR) showExpression(n)
    else showAsk(n, pick)
  })
}

// ---- what it comes to ----
// Yes or no, or one of a list of choices. The wires leaving the node follow
// it: the first choice takes over the "yes" wires and gives them back.
function setChoices(n: ENode, on: boolean) {
  if (on === !!n.choices.length) return
  const first = on ? '选项1' : n.choices[0]
  for (const e of doc.edges) {
    if (e.from !== n.id || e.branch === ELSE) continue
    if (on) e.branch = first
    else if (e.branch === first) delete e.branch
  }
  if (!on) doc.edges = doc.edges.filter((e) => e.from !== n.id || !e.branch || e.branch === ELSE)
  n.choices = on ? [first, '选项2'] : []
  // rows cannot give a choice; an expression can
  if (on && !n.ask) showExpression(n)
}
function renameChoice(n: ENode, i: number, input: HTMLInputElement) {
  const old = n.choices[i]
  const name = input.value.trim()
  if (name === old) return
  if (!name || name === ELSE || n.choices.includes(name)) {
    input.value = old
    return hub.notify(name ? `选项「${name}」不能用：重复了，或者是保留的名字` : '选项不能为空', true)
  }
  for (const e of doc.edges) if (e.from === n.id && e.branch === old) e.branch = name
  n.choices[i] = name
}
function addChoice(n: ENode) {
  let k = n.choices.length + 1
  while (n.choices.includes('选项' + k)) k++
  n.choices.push('选项' + k)
}
function removeChoice(n: ENode, i: number) {
  const [old] = n.choices.splice(i, 1)
  doc.edges = doc.edges.filter((e) => e.from !== n.id || e.branch !== old)
}

// ---- testing against an event ----
// The answer stands for the condition and the event it was given for.
const testResult = ref<null | { matched: boolean; error?: string }>(null)
watch([() => conditionOf(props.node), testEventId], () => {
  testResult.value = null
})
async function test(n: ENode) {
  const id = testEventId.value || scopedEvents.value[0]?.id
  if (!id) return hub.notify('还没有可用于测试的事件', true)
  testEventId.value = id
  testResult.value = (await hub.run(() => post('/rules/test', { condition: conditionOf(n), event_id: id }))) || null
}
</script>

<template>
  <PanelHead :title="`条件 · ${way.name}`" @remove="$emit('remove')" />
  <button
    class="input mb-3 flex w-full cursor-pointer items-center gap-2 text-left"
    title="换一种方式来判断"
    @click="rework"
  >
    <component :is="way.icon" :size="14" class="shrink-0 text-mute" />
    <span class="min-w-0 flex-1 truncate">{{ way.name }}</span>
    <span class="shrink-0 text-xs text-mute">更换</span>
  </button>
  <p class="mb-3 text-xs text-mute">
    <template v-if="node.choices.length">结果是哪个选项，就走哪个选项那一头接出去的节点；都不是就走「其他」。</template>
    <template v-else>成立时走「是」那一头接出去的节点，不成立时走「否」。</template>
    后面可以接动作，也可以再接条件。
  </p>

  <template v-if="node.ask">
    <p v-if="!specOf(node)" class="text-xs text-warn">判断它的接入已经不在了，点上面的「更换」另选一个。</p>
    <SchemaForm
      v-else
      :model-value="node.params || {}"
      :schema="judgeSchema(node)"
      :variables="variablesFor(node)"
      :options-from="node.instance ? `/instances/${node.instance}/options?action=judge` : undefined"
      templated
      @update:model-value="node.params = $event"
    />
    <p v-if="specOf(node)" class="mt-2 text-xs text-mute">
      把要判断的内容和标准写清楚。模型只会回答{{
        node.choices.length ? '下面的选项之一' : '是或否'
      }}，它的答案后面的节点可以用
      <code class="chip" v-text="`{{ steps.${node.id}.choice }}`" />
      引用。事件走到这个节点才会去问，每次问一回。
    </p>
  </template>
  <template v-else-if="node.builder.mode === 'builder'">
    <div
      v-for="(row, i) in node.builder.rows"
      :key="i"
      class="mb-2 flex items-center gap-2"
      style="animation: item-in 0.2s var(--ease-glide)"
    >
      <span class="w-5 shrink-0 text-center text-xs text-mute">{{
        i === 0 ? '当' : node.builder.join === 'and' ? '且' : '或'
      }}</span>
      <input
        v-if="typedField(row)"
        v-model="row.field"
        class="input min-w-0 flex-1 font-mono text-xs"
        placeholder="payload.字段名"
      />
      <select
        v-else
        class="input min-w-0 flex-1"
        :value="row.field"
        @change="chooseField(row, ($event.target as HTMLSelectElement).value)"
      >
        <option v-for="f in fieldHints" :key="f" :value="f" :title="f">{{ fieldLabel(f) }}</option>
        <option :value="OTHER">其他字段…</option>
      </select>
      <select v-model="row.op" class="input w-24">
        <option v-for="o in operators" :key="o.id" :value="o.id">{{ o.label }}</option>
      </select>
      <input
        v-if="!operators.find((o) => o.id === row.op)?.noValue"
        v-model="row.value"
        class="input flex-1"
        :placeholder="row.op === 'in' ? '用逗号分隔' : '值'"
      />
      <button class="btn btn-ghost px-1.5" aria-label="删除这一行" @click="node.builder.rows.splice(i, 1)">
        <Trash2 :size="14" />
      </button>
    </div>
    <div class="flex items-center gap-3">
      <button class="btn" @click="node.builder.rows.push({ field: 'payload.text', op: 'contains', value: '' })">
        <Plus :size="14" />添加一行
      </button>
      <select v-if="node.builder.rows.length > 1" v-model="node.builder.join" class="input w-36">
        <option value="and">全部满足</option>
        <option value="or">满足任一</option>
      </select>
      <span v-if="!node.builder.rows.length" class="text-xs text-mute">一行都没有时总是成立。</span>
    </div>
  </template>
  <template v-else>
    <textarea
      v-model="node.expr"
      class="input font-mono text-xs"
      rows="4"
      placeholder='payload.text contains "deploy" and actor.id in ["123", "456"]'
    />
    <p class="mt-1 text-xs text-mute">
      expr 表达式。可用变量：payload、actor、conversation、type、source、raw，以及前面动作的输出 steps.&lt;标识&gt;。{{
        node.choices.length ? '它算出来的值和哪个选项一样，就走哪个选项。' : '留空表示总是成立。'
      }}
    </p>
  </template>

  <label class="label mt-4">结果</label>
  <div class="seg">
    <button class="!py-0.5 !text-xs" :class="{ on: !node.choices.length }" @click="setChoices(node, false)">
      是 / 否
    </button>
    <button class="!py-0.5 !text-xs" :class="{ on: node.choices.length }" @click="setChoices(node, true)">
      多个选项
    </button>
  </div>
  <template v-if="node.choices.length">
    <div v-for="(c, i) in node.choices" :key="i + ':' + c" class="mt-2 flex items-center gap-2">
      <input
        class="input min-w-0 flex-1"
        :value="c"
        aria-label="选项"
        @change="renameChoice(node, i, $event.target as HTMLInputElement)"
      />
      <button
        class="btn btn-ghost px-1.5"
        aria-label="删除这个选项"
        :disabled="node.choices.length < 2"
        @click="removeChoice(node, i)"
      >
        <Trash2 :size="14" />
      </button>
    </div>
    <button class="btn mt-2" @click="addChoice(node)"><Plus :size="14" />添加选项</button>
    <p class="mt-1 text-xs text-mute">每个选项在节点右边有一个出口，另有一个「其他」。</p>
  </template>

  <TrialResult :step="trialStep(node.id)" :times="trialSteps(node.id).length" />
  <p v-if="node.ask || node.choices.length" class="mt-4 text-xs text-mute">
    用上面的「试运行」拿一个收到过的事件走一遍，看它在这里{{ node.ask ? '被判断成什么' : '走哪个选项' }}。
  </p>
  <div v-else class="mt-4 flex items-center gap-2">
    <select v-model="testEventId" class="input min-w-0 flex-1 text-xs">
      <option value="">用最近一个事件测试</option>
      <option v-for="e in scopedEvents" :key="e.id" :value="e.id">
        {{ clock(e.time) }} · {{ eventTitle(hub.plugins, e) }} · {{ summarize(e).slice(0, 60) }}
      </option>
    </select>
    <button class="btn" @click="test(node)">测试</button>
    <Transition name="swap" mode="out-in">
      <span
        v-if="testResult"
        :key="testResult.error ? 'e' : testResult.matched ? 'y' : 'n'"
        class="flex shrink-0 items-center gap-1.5 text-sm font-medium"
      >
        <span class="dot" :class="testResult.error ? 'bad' : testResult.matched ? 'ok' : ''" />
        {{ testResult.error ? '出错' : testResult.matched ? '是' : '否' }}
      </span>
    </Transition>
  </div>
  <p v-if="testResult?.error && !node.ask && !node.choices.length" class="mt-1 text-xs text-bad">
    {{ testResult.error }}
  </p>
</template>
