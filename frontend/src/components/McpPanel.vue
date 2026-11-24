<script setup lang="ts">
// The other way round from a connection: instead of Cat handing events to an
// AI, an AI is given Cat's tools (its connections, events and rules) through
// an MCP server. It is installed into the harnesses found on this machine,
// each its own way, and taken out again from here.
import { computed, onMounted, ref } from 'vue'
import { Check, Copy, Download, LoaderCircle, Trash2 } from 'lucide-vue-next'
import { useHub } from '../hub'
import { del, get, post } from '../api'

interface Harness {
  id: string
  name: string
  about: string
  found: boolean
  installed: boolean
  // where the entry points, and whether that is somewhere else than here
  url?: string
  stale?: boolean
}
interface State {
  url: string
  harnesses: Harness[]
}
interface ActionFailure {
  id: string
  name: string
  error: string
}
interface BatchResponse extends State {
  changed: string[]
  failed: ActionFailure[]
}

const hub = useHub()
const state = ref<State | null>(null)
const busy = ref('')
const copied = ref(false)

onMounted(async () => (state.value = await get<State>('/mcp/harnesses').catch(() => null)))

const foundHarnesses = computed(() => (state.value?.harnesses || []).filter((h) => h.found))
const installedHarnesses = computed(() => (state.value?.harnesses || []).filter((h) => h.installed))
const installableHarnesses = computed(() => foundHarnesses.value.filter((h) => !h.installed || h.stale))
const onlyStale = computed(
  () => installableHarnesses.value.length > 0 && installableHarnesses.value.every((h) => h.stale),
)

function isRowBusy(h: Harness) {
  if (busy.value === h.id) return true
  if (busy.value === 'install-all' && h.found && (!h.installed || h.stale)) return true
  if (busy.value === 'uninstall-all' && h.installed) return true
  return false
}

async function set(h: Harness, on: boolean) {
  if (busy.value) return
  busy.value = h.id
  const next = await hub.run(
    () => (on ? post<State>(`/mcp/harnesses/${h.id}`) : del<State>(`/mcp/harnesses/${h.id}`)),
    on ? `已装进 ${h.name}，重开它后生效` : `已从 ${h.name} 卸载`,
  )
  if (next) state.value = next
  busy.value = ''
}

async function installAll() {
  if (busy.value || !installableHarnesses.value.length) return
  busy.value = 'install-all'
  const res = await hub.run(() => post<BatchResponse>('/mcp/harnesses/install-all'))
  if (res) {
    state.value = { url: res.url, harnesses: res.harnesses }
    if (res.failed.length) {
      const failText = res.failed.map((f) => `${f.name}（${f.error}）`).join('；')
      const prefix = res.changed.length ? `已安装 ${res.changed.length} 个，` : ''
      hub.notify(`${prefix}失败 ${res.failed.length} 个：${failText}`, true)
    } else if (res.changed.length) {
      hub.notify(`已一键安装到 ${res.changed.join('、')}，重开对应应用后生效`)
    } else {
      hub.notify('已检测到的应用均已安装')
    }
  }
  busy.value = ''
}

async function uninstallAll() {
  if (busy.value || !installedHarnesses.value.length) return
  if (!confirm(`确定从全部 ${installedHarnesses.value.length} 个已安装的 AI 应用中卸载 Cat MCP？`)) return
  busy.value = 'uninstall-all'
  const res = await hub.run(() => post<BatchResponse>('/mcp/harnesses/uninstall-all'))
  if (res) {
    state.value = { url: res.url, harnesses: res.harnesses }
    if (res.failed.length) {
      const failText = res.failed.map((f) => `${f.name}（${f.error}）`).join('；')
      const prefix = res.changed.length ? `已卸载 ${res.changed.length} 个，` : ''
      hub.notify(`${prefix}失败 ${res.failed.length} 个：${failText}`, true)
    } else if (res.changed.length) {
      hub.notify(`已从 ${res.changed.join('、')} 一键卸载`)
    } else {
      hub.notify('没有需要卸载的应用')
    }
  }
  busy.value = ''
}

async function copy() {
  await navigator.clipboard.writeText(state.value!.url)
  copied.value = true
  setTimeout(() => (copied.value = false), 1200)
}
</script>

<template>
  <section v-if="state">
    <div class="mb-3 flex flex-wrap items-start justify-between gap-3">
      <div class="min-w-0 flex-1">
        <div class="flex flex-wrap items-center gap-2">
          <h2 class="text-sm font-semibold">让 AI 用 Cat（MCP）</h2>
          <span class="chip !font-sans tabular-nums">
            已检测 {{ foundHarnesses.length }} 个 · 已安装 {{ installedHarnesses.length }} 个
          </span>
        </div>
        <p class="mt-1 text-xs text-mute">
          把 Cat 作为 MCP 服务装进 AI 应用后，那里的 AI
          就能自己往渠道发消息、查收到的事件、新建和修改规则。在那边说一句「每天 9 点把 RSS
          汇总发到群里」，它会替你把规则建好。
        </p>
      </div>
      <div class="flex shrink-0 items-center gap-2">
        <button
          class="btn btn-primary"
          :disabled="!!busy || !installableHarnesses.length"
          :title="
            installableHarnesses.length
              ? `将 Cat MCP 一键安装到 ${installableHarnesses.length} 个已检测到的应用`
              : foundHarnesses.length
                ? '已检测到的 AI 应用均已安装'
                : '这台电脑上没有找到支持的 AI 应用'
          "
          @click="installAll"
        >
          <LoaderCircle v-if="busy === 'install-all'" :size="14" class="spin" />
          <Download v-else :size="14" />
          {{ busy === 'install-all' ? '正在安装…' : onlyStale ? '一键更新' : '一键安装' }}
        </button>
        <button
          class="btn btn-danger"
          :disabled="!!busy || !installedHarnesses.length"
          :title="
            installedHarnesses.length
              ? `从全部 ${installedHarnesses.length} 个已安装的应用中卸载 Cat MCP`
              : '还没有安装到任何 AI 应用'
          "
          @click="uninstallAll"
        >
          <LoaderCircle v-if="busy === 'uninstall-all'" :size="14" class="spin" />
          <Trash2 v-else :size="14" />
          {{ busy === 'uninstall-all' ? '正在卸载…' : '一键卸载' }}
        </button>
      </div>
    </div>

    <div class="list">
      <div v-for="h in state.harnesses" :key="h.id" class="row flex items-center gap-4 px-4 py-3">
        <div class="min-w-0 flex-1" :class="{ 'opacity-50': !h.found && !h.installed }">
          <div class="flex items-center gap-2 text-sm font-medium">
            {{ h.name }}
            <span v-if="h.installed && !h.stale" class="chip !font-sans text-ok">已安装</span>
            <span v-else-if="h.stale" class="chip !font-sans text-warn">装的是另一个地址</span>
          </div>
          <div class="truncate text-xs text-mute">
            <template v-if="h.stale">它那里的 Cat 指向 {{ h.url }}，不是这一个。重新安装会改成这一个。</template>
            <template v-else-if="!h.found && h.installed">
              未检测到该应用，但配置文件中仍残留 Cat MCP 条目，可直接卸载清理。
            </template>
            <template v-else-if="!h.found">这台电脑上没有找到它</template>
            <template v-else>{{ h.about }}</template>
          </div>
        </div>
        <LoaderCircle v-if="isRowBusy(h)" :size="15" class="spin text-mute" />
        <div v-else-if="h.found || h.installed" class="flex shrink-0 items-center gap-2">
          <button v-if="h.found && (!h.installed || h.stale)" class="btn" :disabled="!!busy" @click="set(h, true)">
            {{ h.stale ? '重新安装' : '安装' }}
          </button>
          <button v-if="h.installed" class="btn btn-danger" :disabled="!!busy" @click="set(h, false)">卸载</button>
        </div>
      </div>
    </div>

    <p class="mt-2.5 flex flex-wrap items-center gap-1 text-xs text-mute">
      别的支持 MCP 的应用，填这个地址（Streamable HTTP）：
      <code class="chip">{{ state.url }}</code>
      <button class="btn btn-ghost px-1.5" title="复制" aria-label="复制" @click="copy">
        <component :is="copied ? Check : Copy" :size="13" />
      </button>
    </p>
    <p class="mt-1 text-xs text-mute">
      装上后那里的 AI 能做你在 Cat 里能做的事，包括用「运行脚本」执行命令。由聊天消息触发的 AI
      默认用不了这些工具，除非你在它的「允许的工具」里放行。
    </p>
  </section>
</template>
