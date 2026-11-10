<script setup lang="ts">
defineProps<{ title: string; hint?: string }>()
</script>

<template>
  <header class="flex items-center justify-between gap-4 border-b border-line bg-surface px-6 py-3.5">
    <div class="min-w-0">
      <h1 class="text-base font-semibold">{{ title }}</h1>
      <p v-if="hint" class="truncate text-xs text-mute">{{ hint }}</p>
    </div>
    <div class="flex shrink-0 items-center gap-2"><slot /></div>
  </header>
</template>
