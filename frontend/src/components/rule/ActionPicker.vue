<script setup lang="ts">
// The list an action is picked from: what can carry one out as it is, what
// is connected, and what could be connected first.
import { computed, ref, type Component } from 'vue'
import { Reply, Search, X } from 'lucide-vue-next'
import { useHub } from '../../hub'
import { SOURCE, kindIcon, kindTitle, useEditor } from '../../editor/context'
import type { Plugin } from '../../types'

defineProps<{ title: string }>()
const emit = defineEmits<{ pick: [instance: string]; connect: [plugin: Plugin]; close: [] }>()
const hub = useHub()
const { targetIcon } = useEditor()

const actionInstances = computed(() => hub.instances.filter((i) => hub.pluginOf(i.plugin)?.actions?.length))
const does = (p?: Plugin) => p?.actions?.map((a) => a.title).join('、') || ''

interface Choice {
  key: string
  name: string
  about: string
  icon: Component
  tag?: string
  instance?: string // something usable as it is
  plugin?: Plugin // something to set up first
}
const search = ref('')
const choices = computed(() => {
  const ready: Choice[] = [
    {
      key: SOURCE,
      name: '原路回复',
      about: '回复到事件来的地方，不管它来自哪个渠道',
      icon: Reply,
      instance: SOURCE,
    },
  ]
  const connected: Choice[] = []
  for (const i of actionInstances.value) {
    const p = hub.pluginOf(i.plugin)!
    const c = { key: i.id, name: i.name, icon: targetIcon(i.id), instance: i.id }
    if (p.singleton) ready.push({ ...c, about: p.description || does(p) })
    else if (hub.given(i)) ready.push({ ...c, about: does(p), tag: kindTitle[p.category] })
    else
      connected.push({ ...c, about: (p.name !== i.name ? p.name + ' · ' : '') + does(p), tag: kindTitle[p.category] })
  }
  const order = ['processor', 'channel', 'monitor']
  const more: Choice[] = hub.plugins
    .filter((p) => !p.singleton && !p.default_instance && p.actions?.length)
    .sort((a, b) => order.indexOf(a.category) - order.indexOf(b.category))
    .map((p) => ({
      key: 'new:' + p.id,
      name: p.name,
      // a monitor's own description is about what it watches; as an action
      // it only offers to be told to check now
      about: p.category === 'monitor' ? `接入后可以让它：${does(p)}` : p.description || does(p),
      icon: kindIcon[p.category],
      tag: kindTitle[p.category],
      plugin: p,
    }))
  const q = search.value.trim().toLowerCase()
  const hit = (c: Choice) => !q || (c.name + c.about + (c.tag || '')).toLowerCase().includes(q)
  return [
    { title: '直接可用', items: ready.filter(hit) },
    { title: '已接入', items: connected.filter(hit) },
    { title: '接入新的', items: more.filter(hit) },
  ].filter((g) => g.items.length)
})
function choose(c: Choice) {
  if (c.instance) emit('pick', c.instance)
  else emit('connect', c.plugin!)
}

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
