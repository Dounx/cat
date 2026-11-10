<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { RotateCcw } from 'lucide-vue-next'
import { useHub, clock, summarize } from '../hub'
import { get, post } from '../api'
import type { HubEvent, Run } from '../types'
import PageHeader from '../components/PageHeader.vue'
import Drawer from '../components/Drawer.vue'
import JsonView from '../components/JsonView.vue'

const hub = useHub()
const source = ref('')
const keyword = ref('')
const selected = ref<HubEvent | null>(null)
const selectedRuns = ref<Run[]>([])

const sources = computed(() => hub.instances.filter((i) => hub.pluginOf(i.plugin)?.capabilities.includes('source')))
const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  return hub.events.filter((e) =>
    (!source.value || e.source === source.value) &&
    (!k || e.type.toLowerCase().includes(k) || JSON.stringify(e.payload).toLowerCase().includes(k) || (e.actor?.name || '').toLowerCase().includes(k)))
})

watch(selected, async (ev) => {
  selectedRuns.value = ev ? await get<Run[]>(`/runs?event_id=${ev.id}`) : []
})
// Keep the open drawer's run list current as runs stream in.
watch(() => hub.runs[0], (run) => {
  if (!run || !selected.value || run.event_id !== selected.value.id) return
  const i = selectedRuns.value.findIndex((r) => r.id === run.id)
  if (i >= 0) selectedRuns.value[i] = run
  else selectedRuns.value.unshift(run)
}, { deep: true })

async function replay() {
  const out = await hub.run(() => post<{ matched: number }>(`/events/${selected.value!.id}/replay`))
  if (out) hub.notify(out.matched ? `已重放，命中 ${out.matched} 条规则` : '已重放，没有规则命中')
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="事件" hint="所有来源收到的事件，实时更新">
      <input v-model="keyword" class="input w-52" placeholder="搜索内容、类型、发送者" />
      <select v-model="source" class="input w-40">
        <option value="">全部来源</option>
        <option v-for="s in sources" :key="s.id" :value="s.id">{{ s.name }}</option>
      </select>
    </PageHeader>

    <div class="flex-1 overflow-y-auto">
      <div v-if="!filtered.length" class="p-16 text-center text-mute">
        <p class="text-sm">还没有事件。</p>
        <p class="mt-1 text-xs">到「接入」里添加一个渠道或监控，例如 WebHook，然后向它发一个请求。</p>
      </div>
      <table v-else class="w-full text-left">
        <tbody>
          <tr v-for="e in filtered" :key="e.id" class="cursor-pointer border-b border-line hover:bg-surface" @click="selected = e">
            <td class="w-28 py-2 pl-6 font-mono text-xs whitespace-nowrap text-mute">{{ clock(e.time) }}</td>
            <td class="w-36 truncate py-2 pr-3 text-sm">{{ hub.instanceName(e.source) }}</td>
            <td class="w-44 py-2 pr-3"><span class="chip">{{ e.type }}</span></td>
            <td class="max-w-0 truncate py-2 pr-6 text-sm">
              <span v-if="e.actor?.name" class="mr-1.5 text-mute">{{ e.actor.name }}：</span>{{ summarize(e) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <Drawer v-if="selected" :title="selected.type" @close="selected = null">
      <template #actions>
        <button class="btn" @click="replay"><RotateCcw :size="14" />重放</button>
      </template>
      <dl class="mb-4 grid grid-cols-[5rem_1fr] gap-y-1.5 text-sm">
        <dt class="text-mute">时间</dt><dd>{{ new Date(selected.time).toLocaleString() }}</dd>
        <dt class="text-mute">来源</dt><dd>{{ hub.instanceName(selected.source) }} <span class="chip ml-1">{{ selected.plugin }}</span></dd>
        <template v-if="selected.actor"><dt class="text-mute">发送者</dt><dd>{{ selected.actor.name || selected.actor.id }} <span class="chip ml-1">{{ selected.actor.id }}</span></dd></template>
        <template v-if="selected.conversation"><dt class="text-mute">会话</dt><dd>{{ selected.conversation.name || selected.conversation.id }} <span class="chip ml-1">{{ selected.conversation.kind }}</span></dd></template>
        <dt class="text-mute">事件 ID</dt><dd class="font-mono text-xs">{{ selected.id }}</dd>
      </dl>
      <h3 class="label">payload</h3>
      <JsonView :value="selected.payload" />
      <h3 class="label mt-4">触发的运行</h3>
      <p v-if="!selectedRuns.length" class="text-xs text-mute">没有规则命中这个事件。</p>
      <RouterLink v-for="r in selectedRuns" :key="r.id" :to="{ path: '/runs', query: { id: r.id } }" class="card mb-1.5 flex items-center justify-between px-3 py-2 text-sm hover:border-accent">
        <span>{{ r.rule_name }}<span v-if="r.replay" class="chip ml-2">重放</span></span>
        <span :class="{ 'text-ok': r.status === 'ok', 'text-bad': r.status === 'error', 'text-warn': r.status === 'running' }">{{ { ok: '成功', error: '失败', running: '运行中' }[r.status] }}</span>
      </RouterLink>
      <details class="mt-4">
        <summary class="label cursor-pointer">原始数据 raw</summary>
        <JsonView :value="selected.raw ?? null" />
      </details>
    </Drawer>
  </div>
</template>
