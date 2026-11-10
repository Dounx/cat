<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ChevronRight, ListChecks } from 'lucide-vue-next'
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
const statusDot: Record<string, string> = { ok: 'ok', error: 'bad', running: 'warn' }
function duration(r: Run): string {
  if (!r.finished_at) return '—'
  const ms = new Date(r.finished_at).getTime() - new Date(r.started_at).getTime()
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="运行记录" hint="每次规则命中后执行的动作及其结果">
      <label class="flex cursor-pointer items-center gap-2 text-sm text-mute"
        ><input v-model="onlyFailed" type="checkbox" />只看失败</label
      >
      <select v-model="ruleId" class="input w-44">
        <option value="">全部规则</option>
        <option v-for="r in hub.rules" :key="r.id" :value="r.id">{{ r.name }}</option>
      </select>
    </PageHeader>

    <div class="gutter flex-1 overflow-y-auto pb-6">
      <div v-if="!filtered.length" class="card empty">
        <ListChecks :size="28" :stroke-width="1.5" />
        <p>{{ hub.runs.length ? '没有符合筛选条件的运行记录。' : '还没有运行记录。' }}</p>
      </div>
      <div v-else class="list">
        <table class="w-full table-fixed text-left">
          <TransitionGroup tag="tbody" name="arrive">
            <tr v-for="r in filtered" :key="r.id" class="cursor-pointer" @click="selectedId = r.id">
              <td class="w-28 py-2.5 pl-4 font-mono text-xs whitespace-nowrap text-mute tabular-nums">
                {{ clock(r.started_at) }}
              </td>
              <td class="w-24 py-2.5 text-sm">
                <span class="flex items-center gap-2"
                  ><span class="dot" :class="statusDot[r.status]" />{{ statusText[r.status] }}</span
                >
              </td>
              <td class="truncate py-2.5 pr-3 text-sm font-medium">
                {{ r.rule_name }}<span v-if="r.replay" class="chip ml-2">重放</span>
              </td>
              <td class="w-1/3 truncate py-2.5 pr-3 text-xs text-bad">{{ r.steps.find((s) => s.error)?.error }}</td>
              <td class="w-20 py-2.5 text-right font-mono text-xs text-mute tabular-nums">{{ duration(r) }}</td>
              <td class="w-9 pr-3 pl-1 text-faint"><ChevronRight :size="15" /></td>
            </tr>
          </TransitionGroup>
        </table>
      </div>
    </div>

    <Transition name="drawer">
      <Drawer v-if="selected" :title="selected.rule_name" wide @close="selectedId = null">
        <p class="mb-4 flex flex-wrap items-center gap-x-2 text-sm text-mute">
          <span class="flex items-center gap-1.5 text-ink"
            ><span class="dot" :class="statusDot[selected.status]" />{{ statusText[selected.status] }}</span
          >
          · {{ new Date(selected.started_at).toLocaleString() }} · {{ duration(selected) }} · 事件
          <span class="font-mono text-xs">{{ selected.event_id }}</span>
        </p>
        <TransitionGroup name="item">
          <div v-for="(s, i) in selected.steps" :key="s.id" class="mb-3 rounded-xl border border-line p-4">
            <div class="mb-2 flex items-center justify-between">
              <div class="flex items-center gap-2 text-sm font-medium">
                <span
                  class="flex h-5 w-5 items-center justify-center rounded-full bg-sunken text-[11px] text-mute tabular-nums"
                  >{{ i + 1 }}</span
                >
                {{ hub.instanceName(s.instance) }} · {{ s.action }} <span class="chip">{{ s.id }}</span>
              </div>
              <div class="flex items-center gap-1.5 font-mono text-xs text-mute">
                <span class="dot" :class="s.error ? 'bad' : 'ok'" />{{ s.error ? '失败' : '成功' }} ·
                {{ s.duration_ms }} ms
              </div>
            </div>
            <p v-if="s.error" class="mb-2 rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">{{ s.error }}</p>
            <h4 class="label">参数</h4>
            <JsonView :value="s.params ?? {}" />
            <h4 class="label mt-3">输出</h4>
            <JsonView :value="s.output ?? null" />
          </div>
        </TransitionGroup>
        <p v-if="selected.status === 'running'" class="flex items-center gap-2 text-sm text-mute">
          <span class="dot warn" />运行中…
        </p>
      </Drawer>
    </Transition>
  </div>
</template>
