<script setup lang="ts">
// What became of a node the last time the rule was tried out.
import type { StepResult } from '../../types'
import JsonView from '../JsonView.vue'

defineProps<{ step?: StepResult }>()
</script>

<template>
  <template v-if="step">
    <h4 class="label mt-4">试运行结果</h4>
    <p v-if="step.error" class="mb-2 rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">{{ step.error }}</p>
    <JsonView v-else :value="step.output ?? null" />
  </template>
</template>
