<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useHub, clock } from '../hub'
import { get } from '../api'
import type { Run } from '../types'
import PageHeader from '../components/PageHeader.vue'
import Drawer from '../components/Drawer.vue'
import JsonView from '../components/JsonView.vue'

const hub = useHub()
const route = useRoute()
const ruleId = ref('')
const onlyFailed = ref(false)
const selectedId = ref<string | null>(null)
const fetched = ref<Run | null>(null)

const filtered = computed(() =>
  hub.runs.filter((r) => (!ruleId.value || r.rule_id === ruleId.value) && (!onlyFailed.value || r.status === 'error')),
)
const selected = computed(
  () =>
    hub.runs.find((r) => r.id === selectedId.value) || (fetched.value?.id === selectedId.value ? fetched.value : null),
)

watch(
  () => route.query.id,
  async (id) => {
    if (typeof id !== 'string') return
    selectedId.value = id
    if (!hub.runs.some((r) => r.id === id)) fetched.value = await get<Run>(`/runs/${id}`).catch(() => null)
  },
  { immediate: true },
)

const statusText: Record<string, string> = { ok: '成功', error: '失败', running: '运行中' }
function duration(r: Run): string {
  if (!r.finished_at) return '—'
  const ms = new Date(r.finished_at).getTime() - new Date(r.started_at).getTime()
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="运行记录" hint="每次规则命中后执行的动作及其结果">
      <label class="flex items-center gap-1.5 text-sm text-mute"
        ><input v-model="onlyFailed" type="checkbox" />只看失败</label
      >
      <select v-model="ruleId" class="input w-44">
        <option value="">全部规则</option>
        <option v-for="r in hub.rules" :key="r.id" :value="r.id">{{ r.name }}</option>
      </select>
    </PageHeader>

    <div class="flex-1 overflow-y-auto">
      <p v-if="!filtered.length" class="p-16 text-center text-sm text-mute">还没有运行记录。</p>
      <table v-else class="w-full text-left">
        <tbody>
          <tr
            v-for="r in filtered"
            :key="r.id"
            class="cursor-pointer border-b border-line hover:bg-surface"
            @click="selectedId = r.id"
          >
            <td class="w-28 py-2 pl-6 font-mono text-xs whitespace-nowrap text-mute">{{ clock(r.started_at) }}</td>
            <td
              class="w-20 py-2 text-sm font-medium"
              :class="{
                'text-ok': r.status === 'ok',
                'text-bad': r.status === 'error',
                'text-warn': r.status === 'running',
              }"
            >
              {{ statusText[r.status] }}
            </td>
            <td class="py-2 pr-3 text-sm">{{ r.rule_name }}<span v-if="r.replay" class="chip ml-2">重放</span></td>
            <td class="max-w-0 w-1/3 truncate py-2 pr-3 text-xs text-bad">{{ r.steps.find((s) => s.error)?.error }}</td>
            <td class="w-24 py-2 pr-6 text-right font-mono text-xs text-mute">{{ duration(r) }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <Drawer v-if="selected" :title="selected.rule_name" wide @close="selectedId = null">
      <p class="mb-4 text-sm text-mute">
        {{ new Date(selected.started_at).toLocaleString() }} · {{ statusText[selected.status] }} ·
        {{ duration(selected) }} · 事件 <span class="font-mono text-xs">{{ selected.event_id }}</span>
      </p>
      <div v-for="(s, i) in selected.steps" :key="s.id" class="card mb-3 p-4">
        <div class="mb-2 flex items-center justify-between">
          <div class="text-sm font-medium">
            {{ i + 1 }}. {{ hub.instanceName(s.instance) }} · {{ s.action }} <span class="chip ml-1">{{ s.id }}</span>
          </div>
          <div class="font-mono text-xs" :class="s.error ? 'text-bad' : 'text-ok'">
            {{ s.error ? '失败' : '成功' }} · {{ s.duration_ms }} ms
          </div>
        </div>
        <p v-if="s.error" class="mb-2 rounded-md bg-bad/10 px-3 py-2 text-sm text-bad">{{ s.error }}</p>
        <h4 class="label">参数</h4>
        <JsonView :value="s.params ?? {}" />
        <h4 class="label mt-3">输出</h4>
        <JsonView :value="s.output ?? null" />
      </div>
      <p v-if="selected.status === 'running'" class="text-sm text-warn">运行中…</p>
    </Drawer>
  </div>
</template>
