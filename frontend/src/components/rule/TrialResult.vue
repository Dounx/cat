<script setup lang="ts">
// What became of a node the last time the rule was tried out.
import type { StepResult } from '../../types'
import JsonView from '../JsonView.vue'

// `times` is how often the node ran, when a loop took the rule through it again.
defineProps<{ step?: StepResult; times?: number }>()
</script>

<template>
  <template v-if="step">
    <h4 class="label mt-4">
      试运行结果<span v-if="(times || 0) > 1" class="font-normal text-mute"
        >（执行了 {{ times }} 次，这是最后一次）</span
      >
    </h4>
    <p v-if="step.error" class="mb-2 rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">{{ step.error }}</p>
    <JsonView v-else :value="step.output ?? null" />
  </template>
</template>
