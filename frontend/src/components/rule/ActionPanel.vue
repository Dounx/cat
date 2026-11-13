<script setup lang="ts">
// The panel of an action node: what carries it out, which of the things that
// can do, and with what.
import { useHub } from '../../hub'
import { useEditor } from '../../editor/context'
import type { ENode } from '../../graph'
import { picksFor } from '../../picks'
import SchemaForm from '../SchemaForm.vue'
import PanelHead from './PanelHead.vue'
import TrialResult from './TrialResult.vue'

const props = defineProps<{ node: ENode }>()
defineEmits<{ remove: [] }>()
const hub = useHub()
const { actionsFor, specOf, targetName, actionTitle, targetIcon, resetParams, variablesFor, trialStep, pickAction } =
  useEditor()

// Change what the node uses, keeping its action if the new one can do it too.
function retarget() {
  pickAction('换一个来执行', (instance) => {
    const n = props.node
    n.instance = instance
    const list = actionsFor(n)
    if (!list.some((a) => a.name === n.action)) n.action = list[0]?.name || ''
    resetParams(n)
  })
}
</script>

<template>
  <PanelHead :title="`${targetName(node)} · ${actionTitle(node)}`" @remove="$emit('remove')" />
  <div class="mb-3 flex items-center gap-2">
    <button
      class="input flex flex-1 cursor-pointer items-center gap-2 text-left"
      title="换一个来执行"
      @click="retarget"
    >
      <component :is="targetIcon(node.instance)" :size="14" class="shrink-0 text-mute" />
      <span class="min-w-0 flex-1 truncate">{{ targetName(node) }}</span>
      <span class="shrink-0 text-xs text-mute">更换</span>
    </button>
    <select v-model="node.action" class="input flex-1" @change="resetParams(node)">
      <option v-for="a in actionsFor(node)" :key="a.name" :value="a.name">{{ a.title }}</option>
    </select>
  </div>
  <p v-if="specOf(node)?.description" class="mb-3 text-xs text-mute">{{ specOf(node)!.description }}</p>
  <SchemaForm
    :model-value="node.params || {}"
    :schema="specOf(node)?.params"
    :variables="variablesFor(node)"
    :picks="picksFor(hub.events, node.instance)"
    :options-from="
      node.instance && node.action ? `/instances/${node.instance}/options?action=${node.action}` : undefined
    "
    templated
    @update:model-value="node.params = $event"
  />
  <details class="mt-3 text-xs text-mute">
    <summary class="cursor-pointer">高级</summary>
    <div class="mt-2 space-y-2">
      <label class="flex cursor-pointer items-center gap-2"
        ><input v-model="node.continue_on_error" type="checkbox" />失败后仍然执行它后面的节点</label
      >
      <p>
        节点标识 <code class="chip">{{ node.id }}</code
        >，后面的节点用 <code class="chip" v-text="`{{ steps.${node.id}.… }}`" /> 引用它的输出。
      </p>
    </div>
  </details>
  <p class="mt-3 text-xs text-mute">用「插入变量」把事件的内容、前面动作的结果放进参数里。</p>
  <TrialResult :step="trialStep(node.id)" />
</template>
