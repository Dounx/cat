<script setup lang="ts">
// The list a node is picked from, by name: what it offers comes in groups,
// and each entry is used as it is or stands for something to connect first.
import { computed, ref } from 'vue'
import { Search, X } from 'lucide-vue-next'
import type { Choice, ChoiceGroup } from '../../editor/picking'

const props = defineProps<{ title: string; groups: ChoiceGroup[] }>()
const emit = defineEmits<{ pick: [choice: Choice]; close: [] }>()

const search = ref('')
const choices = computed(() => {
  const q = search.value.trim().toLowerCase()
  const hit = (c: Choice) => !q || (c.name + c.about + (c.tag || '')).toLowerCase().includes(q)
  return props.groups.map((g) => ({ title: g.title, items: g.items.filter(hit) })).filter((g) => g.items.length)
})
const choose = (c: Choice) => emit('pick', c)

// Put the cursor in the search field as soon as it shows.
const vFocus = { mounted: (el: HTMLElement) => el.focus() }
</script>

<template>
  <div class="scrim fixed inset-0 z-50 flex items-start justify-center p-4 pt-[12vh]" @click.self="$emit('close')">
    <section class="pop flex max-h-[70vh] w-full max-w-lg flex-col overflow-hidden">
      <header class="flex items-center gap-2 border-b border-line px-4 py-2.5">
        <Search :size="15" class="shrink-0 text-mute" />
        <input
          v-model="search"
          v-focus
          class="min-w-0 flex-1 bg-transparent py-1 text-sm !outline-none placeholder:text-mute/70"
          :placeholder="title + '，可以输入名字查找'"
          @keydown.enter="choices[0] && choose(choices[0].items[0])"
        />
        <button class="btn btn-ghost px-1.5" aria-label="关闭" @click="$emit('close')"><X :size="16" /></button>
      </header>
      <div class="flex-1 overflow-y-auto p-2">
        <template v-for="g in choices" :key="g.title">
          <div class="px-3 pt-2 pb-1 text-[11px] font-medium text-mute">{{ g.title }}</div>
          <button v-for="c in g.items" :key="c.key" class="choice" @click="choose(c)">
            <span class="glyph"><component :is="c.icon" :size="15" /></span>
            <span class="min-w-0 flex-1">
              <span class="flex items-center gap-2 text-sm font-medium">
                {{ c.name }}<span v-if="c.tag" class="chip !font-sans">{{ c.tag }}</span>
              </span>
              <span class="block truncate text-xs text-mute">{{ c.about }}</span>
            </span>
            <span v-if="c.plugin" class="shrink-0 text-xs text-accent">接入</span>
          </button>
        </template>
        <p v-if="!choices.length" class="px-3 py-6 text-center text-sm text-mute">没有找到「{{ search }}」</p>
      </div>
    </section>
  </div>
</template>

<style scoped>
.choice {
  display: flex;
  width: 100%;
  align-items: center;
  gap: 12px;
  padding: 7px 12px;
  border-radius: 9px;
  text-align: left;
  cursor: pointer;
  outline: none;
}
.choice:hover,
.choice:focus-visible {
  background: var(--hover);
}
.choice .glyph {
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
</style>
