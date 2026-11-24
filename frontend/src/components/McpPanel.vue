<script setup lang="ts">
// The other way round from a connection: instead of Cat handing events to an
// AI, an AI is given Cat's tools (its connections, events and rules) through
// an MCP server. It is installed into the harnesses found on this machine,
// each its own way, and taken out again from here.
import { onMounted, ref } from 'vue'
import { Check, Copy, LoaderCircle } from 'lucide-vue-next'
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

const hub = useHub()
const state = ref<State | null>(null)
const busy = ref('')
const copied = ref(false)

onMounted(async () => (state.value = await get<State>('/mcp/harnesses').catch(() => null)))

async function set(h: Harness, on: boolean) {
  busy.value = h.id
  const next = await hub.run(
    () => (on ? post<State>(`/mcp/harnesses/${h.id}`) : del<State>(`/mcp/harnesses/${h.id}`)),
    on ? `已装进 ${h.name}，重开它后生效` : `已从 ${h.name} 卸载`,
  )
  if (next) state.value = next
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
    <h2 class="text-sm font-semibold">让 AI 用 Cat</h2>
    <p class="mt-1 mb-3 text-xs text-mute">
      把 Cat 作为 MCP 服务装进 AI 应用后，那里的 AI
      就能自己往渠道发消息、查收到的事件、新建和修改规则。在那边说一句「每天 9 点把 RSS
      汇总发到群里」，它会替你把规则建好。
    </p>
    <div class="list">
      <div v-for="h in state.harnesses" :key="h.id" class="row flex items-center gap-4 px-4 py-3">
        <div class="min-w-0 flex-1" :class="{ 'opacity-50': !h.found }">
          <div class="flex items-center gap-2 text-sm font-medium">
            {{ h.name }}
            <span v-if="h.installed && !h.stale" class="chip !font-sans">已安装</span>
            <span v-else-if="h.stale" class="chip !font-sans text-warn">装的是另一个地址</span>
          </div>
          <div class="truncate text-xs text-mute">
            <template v-if="!h.found">这台电脑上没有找到它</template>
            <template v-else-if="h.stale">它那里的 Cat 指向 {{ h.url }}，不是这一个。重新安装会改成这一个。</template>
            <template v-else>{{ h.about }}</template>
          </div>
        </div>
        <LoaderCircle v-if="busy === h.id" :size="15" class="spin text-mute" />
        <template v-else-if="h.found">
          <button v-if="!h.installed || h.stale" class="btn" @click="set(h, true)">
            {{ h.stale ? '重新安装' : '安装' }}
          </button>
          <button v-if="h.installed" class="btn btn-ghost btn-danger" @click="set(h, false)">卸载</button>
        </template>
      </div>
    </div>
    <p class="mt-2 flex items-center gap-1 text-xs text-mute">
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
