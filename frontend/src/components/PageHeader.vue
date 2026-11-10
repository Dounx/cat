<script setup lang="ts">
defineProps<{ title: string; hint?: string }>()
</script>

<template>
  <header class="gutter flex shrink-0 items-end justify-between gap-4 pt-3 pb-4">
    <div class="min-w-0">
      <h1 class="text-lg font-semibold tracking-tight">{{ title }}</h1>
      <p v-if="hint" class="truncate text-[13px] text-mute">{{ hint }}</p>
    </div>
    <div class="flex shrink-0 items-center gap-2"><slot /></div>
  </header>
</template>
