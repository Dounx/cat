<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { Check, ChevronRight, Copy, LoaderCircle, Pencil, Plus, RefreshCw, Trash2 } from 'lucide-vue-next'
import { useHub } from '../hub'
import { del, get, post, put } from '../api'
import type { Instance, Plugin } from '../types'
import PageHeader from '../components/PageHeader.vue'
import Drawer from '../components/Drawer.vue'
import SchemaForm from '../components/SchemaForm.vue'
import SetupWizard from '../components/SetupWizard.vue'
import { picksFor } from '../picks'

const hub = useHub()
const router = useRouter()
const editing = ref<null | {
  plugin: Plugin
  id?: string
  name: string
  enabled: boolean
  config: Record<string, any>
}>(null)

type Category = Plugin['category']
const categories: { id: Category; title: string; hint: string; empty: string }[] = [
  {
    id: 'channel',
    title: '渠道',
    hint: '消息从哪里来，回复和通知发到哪里去',
    empty: '还没有接入任何渠道',
  },
  {
    id: 'monitor',
    title: '监控',
    hint: '盯着外面的变化，有动静时触发规则',
    empty: '还没有添加监控',
  },
  {
    id: 'processor',
    title: 'AI',
    hint: '把事情交给谁去想、去做，再把结果拿回来',
    empty: '还没有接入 AI',
  },
]
// The page shows what is set up; what could be added waits behind each
// section's 添加 button, and the market behind its own tab. Singleton built-ins
// (HTTP 请求, 运行脚本) are plain actions with nothing to set up, and the
// monitors that are told what to watch by the rules themselves (定时触发,
// 网页变动, 接口变动) come ready, so both only appear where they are used: in
// the rule editor.
const tab = ref<'instances' | 'market'>('instances')
const picking = ref<Category | null>(null)
const pickingGroup = computed(() => groups.value.find((g) => g.id === picking.value))

function categoryOf(i: Instance): Category {
  return hub.pluginOf(i.plugin)?.category ?? 'channel'
}
const groups = computed(() =>
  categories.map((c) => ({
    ...c,
    addable: hub.plugins.filter((p) => !p.singleton && !p.default_instance && p.category === c.id),
    configured: hub.instances.filter((i) => !hub.given(i) && categoryOf(i) === c.id),
    // what of this kind needs no connecting: said, so that it is not looked for here
    given: hub.plugins.filter((p) => p.default_instance && p.category === c.id).map((p) => p.name),
    market: market.value.plugins.filter((p) => p.category === c.id),
  })),
)

const statusText: Record<string, string> = { running: '运行中', starting: '启动中', stopped: '已停用', error: '出错' }
const statusDot: Record<string, string> = { running: 'ok', starting: 'warn', stopped: '', error: 'bad' }

function ingressURL(i: Instance) {
  return `${location.origin}/ingress/${i.id}`
}
// The copy icon turns into a tick for a moment where it was pressed.
const copied = ref('')
async function copy(text: string) {
  await navigator.clipboard.writeText(text)
  copied.value = text
  setTimeout(() => {
    if (copied.value === text) copied.value = ''
  }, 1400)
}
const restarting = ref('')

interface MarketPlugin {
  id: string
  name: string
  version: string
  description?: string
  homepage?: string
  category: Category
  supported: boolean
  installed_version: string
  bundled: boolean
}
const market = ref<{ configured: boolean; error?: string; plugins: MarketPlugin[] }>({ configured: false, plugins: [] })
const installing = ref('')
const progress = computed(() => {
  const d = hub.signals.download as { plugin: string; done: number; total: number } | undefined
  if (!d || d.plugin !== installing.value) return ''
  return d.total > 0 ? `${Math.round((d.done / d.total) * 100)}%` : `${(d.done / 1e6).toFixed(1)} MB`
})
async function loadMarket() {
  market.value = await get('/market').catch(() => market.value)
}
onMounted(loadMarket)
watch(
  () => hub.signals.plugins,
  () => {
    hub.reloadPlugins()
    loadMarket()
  },
)

async function install(p: MarketPlugin) {
  const verb = p.installed_version ? '更新' : '安装'
  if (
    !confirm(
      `${verb}插件「${p.name}」${p.version}？\n\n插件以你的用户身份运行，没有沙箱，能做你能做的任何事。请只安装你信任的插件。`,
    )
  )
    return
  installing.value = p.id
  await hub.run(() => post(`/market/${p.id}/install`), `已${verb} ${p.name}`)
  installing.value = ''
  await Promise.all([hub.reloadPlugins(), loadMarket()])
}
async function uninstall(p: MarketPlugin) {
  if (!confirm(`卸载插件「${p.name}」？`)) return
  await hub.run(() => del(`/market/${p.id}`), '已卸载')
  await Promise.all([hub.reloadPlugins(), loadMarket()])
}

const wizard = ref<null | { plugin: Plugin; instanceId?: string }>(null)

function closeWizard() {
  wizard.value = null
  hub.reloadInstances()
}
async function wizardToRule() {
  wizard.value = null
  await hub.reloadInstances()
  router.push({ path: '/rules', query: { new: '' } })
}
// Something is connected and nothing uses it yet: say what comes next.
const idle = computed(() => !hub.rules.length && hub.instances.some((i) => !hub.given(i)))

function add(p: Plugin) {
  picking.value = null
  if (p.setup) wizard.value = { plugin: p }
  else editing.value = { plugin: p, name: p.name, enabled: true, config: {} }
}
function rerunSetup() {
  const e = editing.value!
  editing.value = null
  wizard.value = { plugin: e.plugin, instanceId: e.id }
}
function edit(i: Instance) {
  const p = hub.pluginOf(i.plugin)
  if (p) editing.value = { plugin: p, id: i.id, name: i.name, enabled: i.enabled, config: { ...i.config } }
}
async function save() {
  const e = editing.value!
  const body = { plugin: e.plugin.id, name: e.name, enabled: e.enabled, config: e.config }
  const ok = await hub.run(() => (e.id ? put(`/instances/${e.id}`, body) : post('/instances', body)), '已保存')
  if (ok) {
    editing.value = null
    hub.reloadInstances()
  }
}
async function toggle(i: Instance) {
  await hub.run(() => put(`/instances/${i.id}`, { name: i.name, enabled: !i.enabled, config: i.config }))
  hub.reloadInstances()
}
async function restart(i: Instance) {
  restarting.value = i.id
  await hub.run(() => post(`/instances/${i.id}/restart`), '已重启')
  restarting.value = ''
}
async function remove(i: Instance) {
  if (!confirm(`删除「${i.name}」？用到它的规则将无法执行对应的动作。`)) return
  await hub.run(() => del(`/instances/${i.id}`), '已删除')
  hub.reloadInstances()
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="接入" hint="把聊天软件、要盯的东西和 AI 接进来，规则才有东西可用">
      <div class="seg">
        <button :class="{ on: tab === 'instances' }" @click="tab = 'instances'">已接入</button>
        <button :class="{ on: tab === 'market' }" @click="tab = 'market'">插件市场</button>
      </div>
    </PageHeader>

    <div v-if="tab === 'instances'" class="gutter flex-1 space-y-8 overflow-y-auto pb-8">
      <RouterLink
        v-if="idle"
        :to="{ path: '/rules', query: { new: '' } }"
        class="card card-link flex items-center gap-3 px-4 py-3 text-sm"
      >
        <span class="min-w-0 flex-1">
          接好了，但还没有规则用到它们。<span class="text-mute">接入只负责连上，要它做事得建一条规则。</span>
        </span>
        <span class="shrink-0 text-accent">建一条规则</span>
        <ChevronRight :size="15" class="shrink-0 text-faint" />
      </RouterLink>
      <section v-for="g in groups" :key="g.id">
        <div class="mb-2.5 flex items-end justify-between gap-4">
          <div class="min-w-0">
            <h2 class="text-sm font-semibold">{{ g.title }}</h2>
            <p class="truncate text-xs text-mute">{{ g.hint }}</p>
            <p v-if="g.given.length" class="truncate text-xs text-mute">
              {{ g.given.join('、') }}不用接入，建规则时在触发节点里直接填。
            </p>
          </div>
          <button class="btn shrink-0" @click="picking = g.id"><Plus :size="14" />添加</button>
        </div>

        <button
          v-if="!g.configured.length"
          class="card card-link w-full border-dashed px-6 py-5 text-center text-sm text-mute"
          @click="picking = g.id"
        >
          {{ g.empty }}，<span class="text-accent">添加一个</span>
        </button>
        <TransitionGroup v-else tag="div" name="item" class="list">
          <div v-for="i in g.configured" :key="i.id" class="row flex items-center gap-3 px-4 py-3">
            <div class="min-w-0 flex-1 transition-opacity duration-200" :class="{ 'opacity-55': !i.enabled }">
              <div class="flex items-center gap-2 text-sm font-medium">
                <span class="truncate">{{ i.name }}</span>
                <span v-if="(hub.pluginOf(i.plugin)?.name || i.plugin) !== i.name" class="chip shrink-0">{{
                  hub.pluginOf(i.plugin)?.name || i.plugin
                }}</span>
              </div>
              <div class="mt-0.5 flex min-w-0 items-center gap-1.5 text-xs text-mute">
                <span class="dot" :class="statusDot[i.status]" />
                <span class="shrink-0">{{ statusText[i.status] }}</span>
                <span v-if="i.error" class="truncate text-bad" :title="i.error">· {{ i.error }}</span>
                <button
                  v-else-if="hub.pluginOf(i.plugin)?.ingress"
                  class="flex min-w-0 cursor-pointer items-center gap-1 font-mono transition-colors hover:text-accent"
                  title="复制地址"
                  @click="copy(ingressURL(i))"
                >
                  <span class="truncate">· {{ ingressURL(i) }}</span>
                  <Check v-if="copied === ingressURL(i)" :size="12" class="tick shrink-0 text-ok" />
                  <Copy v-else :size="12" class="shrink-0" />
                </button>
              </div>
            </div>
            <div class="reveal flex items-center">
              <button class="btn btn-ghost px-2" title="编辑" @click="edit(i)"><Pencil :size="15" /></button>
              <button class="btn btn-ghost px-2" title="重启" @click="restart(i)">
                <RefreshCw :size="15" :class="{ spin: restarting === i.id }" />
              </button>
              <button class="btn btn-ghost btn-danger px-2" title="删除" @click="remove(i)">
                <Trash2 :size="15" />
              </button>
            </div>
            <input type="checkbox" :checked="i.enabled" :title="i.enabled ? '停用' : '启用'" @change="toggle(i)" />
          </div>
        </TransitionGroup>
      </section>
    </div>

    <div v-else class="gutter flex-1 overflow-y-auto pb-8">
      <p class="text-xs text-mute">插件以你的用户身份运行，没有沙箱。安装前会校验文件的 SHA-256。</p>
      <p v-if="market.error" class="card mt-3 px-6 py-7 text-center text-sm text-mute">{{ market.error }}</p>
      <template v-for="g in groups" :key="g.id">
        <template v-if="g.market.length">
          <h3 class="section-title mt-5 mb-2">{{ g.title }}</h3>
          <div class="list">
            <div v-for="p in g.market" :key="p.id" class="row flex min-h-14 items-center gap-4 px-4 py-2.5">
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2 text-sm font-medium">
                  {{ p.name }}<span class="chip">{{ p.version }}</span>
                  <span v-if="p.bundled" class="text-xs font-normal text-mute">随应用提供</span>
                  <span v-else-if="p.installed_version" class="text-xs font-normal text-ok"
                    >已安装 {{ p.installed_version }}</span
                  >
                </div>
                <p class="truncate text-xs text-mute">{{ p.description }}</p>
              </div>
              <span v-if="installing === p.id" class="flex items-center gap-1.5 text-xs text-mute tabular-nums"
                ><LoaderCircle :size="14" class="spin" />下载中 {{ progress }}</span
              >
              <template v-else-if="!p.bundled">
                <span v-if="!p.supported" class="text-xs text-mute">不支持当前系统</span>
                <button v-else-if="!p.installed_version" class="btn" @click="install(p)">安装</button>
                <template v-else>
                  <button v-if="p.installed_version !== p.version" class="btn btn-primary" @click="install(p)">
                    更新
                  </button>
                  <button class="btn btn-ghost btn-danger" @click="uninstall(p)">卸载</button>
                </template>
              </template>
            </div>
          </div>
        </template>
      </template>
    </div>

    <Transition name="drawer">
      <Drawer v-if="pickingGroup" :title="'添加' + pickingGroup.title" @close="picking = null">
        <p class="mb-4 text-sm text-mute">{{ pickingGroup.hint }}</p>
        <div v-if="pickingGroup.addable.length" class="list">
          <button
            v-for="p in pickingGroup.addable"
            :key="p.id"
            class="row link flex w-full cursor-pointer items-center gap-3 px-4 py-3 text-left"
            @click="add(p)"
          >
            <div class="min-w-0 flex-1">
              <div class="text-sm font-medium">{{ p.name }}</div>
              <p class="mt-0.5 text-xs text-mute">{{ p.description }}</p>
            </div>
            <ChevronRight :size="16" class="shrink-0 text-faint" />
          </button>
        </div>
        <p v-else class="text-sm text-mute">还没有这一类插件。</p>
        <button
          class="mt-4 cursor-pointer text-xs text-mute hover:text-accent"
          @click="((picking = null), (tab = 'market'))"
        >
          没有想要的？去插件市场看看
        </button>
      </Drawer>
    </Transition>

    <Transition name="drawer">
      <Drawer v-if="editing" :title="(editing.id ? '编辑 ' : '添加 ') + editing.plugin.name" @close="editing = null">
        <p class="mb-4 text-sm text-mute">{{ editing.plugin.description }}</p>
        <div class="space-y-3">
          <div><label class="label">名称</label><input v-model="editing.name" class="input" /></div>
          <SchemaForm
            v-model="editing.config"
            :schema="editing.plugin.config_schema"
            :picks="picksFor(hub.events)"
            :options-from="editing.id ? `/instances/${editing.id}/options?action=` : undefined"
          />
        </div>
        <template #footer>
          <button v-if="editing.id && editing.plugin.setup" class="btn mr-auto" @click="rerunSetup">
            重新运行接入向导
          </button>
          <button class="btn" @click="editing = null">取消</button>
          <button class="btn btn-primary" @click="save">保存</button>
        </template>
      </Drawer>
    </Transition>

    <Transition name="modal">
      <SetupWizard
        v-if="wizard"
        :plugin="wizard.plugin"
        :instance-id="wizard.instanceId"
        @close="closeWizard"
        @rule="wizardToRule"
      />
    </Transition>
  </div>
</template>
