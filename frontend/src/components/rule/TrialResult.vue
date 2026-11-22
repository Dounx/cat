<script setup lang="ts">
// What became of a node the last time the rule was tried out.
import { computed } from 'vue'
import type { StepResult } from '../../types'
import JsonView from '../JsonView.vue'

// `times` is how often the node ran, when a loop took the rule through it again.
const props = defineProps<{ step?: StepResult; times?: number }>()
const hasParams = computed(() => props.step?.params && Object.keys(props.step.params).length > 0)
</script>

<template>
  <template v-if="step">
    <div class="mt-4 mb-1.5 flex items-center gap-2">
      <h4 class="label !mb-0">
        试运行结果<span v-if="(times || 0) > 1" class="font-normal text-mute"
          >（执行了 {{ times }} 次，这是最后一次）</span
        >
      </h4>
      <span
        v-if="step.dry_run"
        class="inline-flex items-center rounded-full bg-accent-soft px-2 py-0.5 text-[11px] font-medium text-accent"
      >
        仅演练
      </span>
      <span
        v-else-if="!step.error"
        class="inline-flex items-center rounded-full bg-ok/12 px-2 py-0.5 text-[11px] font-medium text-ok"
      >
        真实执行
      </span>
    </div>
    <p v-if="step.dry_run && !step.error" class="mb-2.5 rounded-lg bg-accent-soft px-3 py-2 text-xs text-accent">
      演练模式未真实调用该动作，下面展示模板渲染后将要发送的参数与模拟输出。
    </p>
    <p v-if="step.error" class="mb-2 rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">{{ step.error }}</p>
    <p v-if="step.held && typeof step.output?.note === 'string' && step.output.note" class="mb-2 text-xs text-mute">
      {{ step.output.note }}
    </p>
    <p v-if="step.attempts" class="mb-2 text-xs text-mute">试了 {{ step.attempts }} 次。</p>
    <template v-if="hasParams">
      <h5 class="label mt-2">{{ step.dry_run ? '将要发送的参数（模板已渲染）' : '参数' }}</h5>
      <JsonView :value="step.params" />
    </template>
    <template v-if="!step.error">
      <h5 class="label mt-2.5">{{ step.dry_run ? '模拟输出（供下游模板引用）' : '输出' }}</h5>
      <JsonView :value="step.output ?? null" />
    </template>
  </template>
</template>
