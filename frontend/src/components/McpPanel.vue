<script setup lang="ts">
// The other way round from a connection: instead of Cat handing events to an
// AI, an AI is given Cat's tools (its connections, events and rules) through
// an MCP server. It is installed into the harnesses found on this machine,
// each its own way, and taken out again from here.
import { onMounted, ref } from 'vue'
import { Check, Copy, LoaderCircle } from 'lucide-vue-next'
import { useHub } from '../hub'
import { del, get, post, put } from '../api'

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
interface Policy {
  allow_write_rules: boolean
  allow_invoke_actions: boolean
  allow_shell: boolean
}
interface State {
  url: string
  harnesses: Harness[]
  policy: Policy
}

const hub = useHub()
const state = ref<State | null>(null)
const busy = ref('')
const savingPolicy = ref(false)
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

async function setPolicy(key: keyof Policy, ev: Event) {
  if (!state.value) return
  const input = ev.target as HTMLInputElement
  const prev = state.value.policy[key]
  const nextPolicy: Policy = { ...state.value.policy, [key]: input.checked }
  savingPolicy.value = true
  const saved = await hub.run(() => put<Policy>('/mcp/policy', nextPolicy), '已保存 MCP 权限设置')
  if (saved) {
    state.value.policy = saved
  } else {
    input.checked = prev
  }
  savingPolicy.value = false
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

    <div v-if="state.policy" class="mt-4">
      <h3 class="mb-2 text-xs font-semibold text-mute">权限控制</h3>
      <div class="list">
        <label class="row flex cursor-pointer items-center justify-between gap-4 px-4 py-2.5">
          <div class="min-w-0">
            <div class="text-sm font-medium">允许创建和修改规则</div>
            <div class="text-xs text-mute">允许 AI 保存、启用/停用或删除规则；关闭后仅可查看现有规则</div>
          </div>
          <input
            type="checkbox"
            aria-label="允许创建和修改规则"
            :checked="state.policy.allow_write_rules"
            :disabled="savingPolicy"
            @change="setPolicy('allow_write_rules', $event)"
          />
        </label>
        <label class="row flex cursor-pointer items-center justify-between gap-4 px-4 py-2.5">
          <div class="min-w-0">
            <div class="text-sm font-medium">允许执行动作和试运行</div>
            <div class="text-xs text-mute">允许 AI 调用接入动作、试运行规则、重放事件和取消运行</div>
          </div>
          <input
            type="checkbox"
            aria-label="允许执行动作和试运行"
            :checked="state.policy.allow_invoke_actions"
            :disabled="savingPolicy"
            @change="setPolicy('allow_invoke_actions', $event)"
          />
        </label>
        <label class="row flex cursor-pointer items-center justify-between gap-4 px-4 py-2.5">
          <div class="min-w-0">
            <div class="flex items-center gap-2 text-sm font-medium">
              允许调用「运行脚本（shell）」
              <span class="chip !font-sans" :class="state.policy.allow_shell ? 'text-warn' : ''">
                {{ state.policy.allow_shell ? '高风险 · 已开启' : '默认关闭' }}
              </span>
            </div>
            <div class="text-xs text-mute">
              关闭时禁止直接执行 shell 命令，也禁止创建或试运行含 shell 节点的规则；仅在完全信任当前 AI 时开启
            </div>
          </div>
          <input
            type="checkbox"
            aria-label="允许调用「运行脚本（shell）」"
            :checked="state.policy.allow_shell"
            :disabled="savingPolicy"
            @change="setPolicy('allow_shell', $event)"
          />
        </label>
      </div>
    </div>

    <p class="mt-2 text-xs text-mute">
      查看接入、事件、运行记录和实例日志始终可用；「运行脚本（shell）」默认禁止，需在上方显式开启。由聊天消息触发的 AI
      默认用不了这些工具，除非你在它的「允许的工具」里放行。
    </p>
  </section>
</template>
