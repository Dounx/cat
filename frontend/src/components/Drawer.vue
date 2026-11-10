<script setup lang="ts">
defineProps<{ title: string; wide?: boolean }>()
defineEmits<{ close: [] }>()
</script>

<template>
  <div class="fixed inset-0 z-40 flex justify-end bg-black/30" @click.self="$emit('close')">
    <section
      class="flex h-full w-full flex-col border-l border-line bg-bg shadow-xl"
      :class="wide ? 'max-w-3xl' : 'max-w-xl'"
    >
      <header class="flex items-center justify-between gap-3 border-b border-line bg-surface px-5 py-3">
        <h2 class="truncate text-[15px] font-semibold">{{ title }}</h2>
        <div class="flex items-center gap-2">
          <slot name="actions" />
          <button class="btn btn-ghost px-2" aria-label="关闭" @click="$emit('close')">✕</button>
        </div>
      </header>
      <div class="flex-1 overflow-y-auto p-5"><slot /></div>
      <footer v-if="$slots.footer" class="flex justify-end gap-2 border-t border-line bg-surface px-5 py-3">
        <slot name="footer" />
      </footer>
    </section>
  </div>
</template>
