<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  Check,
  ChevronRight,
  Copy,
  Download,
  LoaderCircle,
  Pencil,
  Plus,
  RefreshCw,
  Search,
  Trash2,
} from 'lucide-vue-next'
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
// One list: what a thing is for shows in what it is, and a rule may use the
// same connection to listen, to act, or both. The kinds only order the lists,
// so that like still sits next to like.
const order: Category[] = ['channel', 'monitor', 'processor']
const byKind = <T,>(list: T[], kind: (x: T) => Category | undefined) =>
  [...list].sort((a, b) => order.indexOf(kind(a) ?? 'channel') - order.indexOf(kind(b) ?? 'channel'))
// The page shows what is set up; what could be added waits behind the 添加
// button: the plugins there are, and the ones the market has, which are
// installed on the way to connecting them. Singleton built-ins (HTTP 请求,
// 运行脚本) are plain actions with nothing to set up, and the monitors that are
// told what to watch by the rules themselves (定时触发, 网页变动, 接口变动)
// come ready, so both only appear where they are used: in the rule editor.
const picking = ref<false | 'add' | 'plugins'>(false)

const addable = computed(() =>
  byKind(
    hub.plugins.filter((p) => !p.singleton && !p.default_instance),
    (p) => p.category,
  ),
)
const configured = computed(() =>
  byKind(
    hub.instances.filter((i) => !hub.given(i)),
    (i) => hub.pluginOf(i.plugin)?.category,
  ),
)
// what needs no connecting: said, so that it is not looked for here
const given = computed(() => hub.plugins.filter((p) => p.default_instance).map((p) => p.name))
// What the market has that is not here yet, and what came from it: the
// latter can be updated and removed.
const offered = computed(() =>
  byKind(
    market.value.plugins.filter((p) => !p.installed_version),
    (p) => p.category,
  ),
)
// The sheet lists a dozen plugins and more with a market: it is searched by
// name and by what a plugin says it does, and narrowed to one kind.
const query = ref('')
const kind = ref<Category | ''>('')
const kinds: { id: Category | ''; title: string }[] = [
  { id: '', title: '全部' },
  { id: 'channel', title: '渠道' },
  { id: 'monitor', title: '监控' },
  { id: 'processor', title: 'AI' },
]
const searchEl = ref<HTMLInputElement | null>(null)
watch(picking, (now, was) => {
  if (now === 'add' && !was) query.value = kind.value = ''
  // typing searches at once, coming back from the installed plugins as well
  if (now === 'add') nextTick(() => searchEl.value?.focus())
})
function sought(p: { id: string; name: string; description?: string; category: Category }) {
  if (kind.value && p.category !== kind.value) return false
  const q = query.value.trim().toLowerCase()
  return !q || [p.name, p.id, p.description || ''].some((t) => t.toLowerCase().includes(q))
}
const foundAddable = computed(() => addable.value.filter(sought))
const foundOffered = computed(() => offered.value.filter(sought))

const installed = computed(() => market.value.plugins.filter((p) => p.installed_version && !p.bundled))
const outdated = computed(() => installed.value.filter((p) => p.installed_version !== p.version))

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
    return false
  installing.value = p.id
  const ok = await hub.run(() => post(`/market/${p.id}/install`), `已${verb} ${p.name}`)
  installing.value = ''
  await Promise.all([hub.reloadPlugins(), loadMarket()])
  return !!ok
}
// Installing is a step on the way: what was asked for is the connection.
async function installAndAdd(p: MarketPlugin) {
  if (!p.supported || installing.value || !(await install(p))) return
  const plugin = hub.pluginOf(p.id)
  if (plugin && !plugin.singleton && !plugin.default_instance) add(plugin)
  else picking.value = false
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
// What a connection is for: the rules that name it. One that only sends
// events may be named by none and still be heard, by the rules that listen
// to everything.
function heardByAll(i: Instance) {
  return hub.pluginOf(i.plugin)?.capabilities.includes('source') ? hub.rulesHearingAll.length : 0
}

function add(p: Plugin) {
  picking.value = false
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
  const users = hub.rulesUsing(i.id).map((r) => `· ${r.name || '未命名规则'}`)
  const what = users.length
    ? `这些规则用到它，删除后它们的这一部分不再起作用：\n${users.join('\n')}`
    : '没有规则指定用它。'
  if (!confirm(`删除「${i.name}」？\n\n${what}`)) return
  await hub.run(() => del(`/instances/${i.id}`), '已删除')
  hub.reloadInstances()
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="接入" hint="把聊天软件、要盯的东西和 AI 接进来，规则才有东西可用">
      <button class="btn btn-primary" @click="picking = 'add'"><Plus :size="15" />添加</button>
    </PageHeader>

    <div class="gutter flex-1 space-y-3 overflow-y-auto pb-8">
      <button
        v-if="!configured.length"
        class="card card-link w-full border-dashed px-6 py-5 text-center text-sm text-mute"
        @click="picking = 'add'"
      >
        还没有接入任何东西，<span class="text-accent">添加一个</span>
      </button>
      <TransitionGroup v-else tag="div" name="item" class="list">
        <div v-for="i in configured" :key="i.id" class="row flex items-center gap-3 px-4 py-3">
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
          <div class="max-w-[40%] min-w-0 truncate text-xs text-mute">
            <template v-if="hub.rulesUsing(i.id).length">
              用于
              <template v-for="(r, n) in hub.rulesUsing(i.id)" :key="r.id">
                <template v-if="n">、</template>
                <RouterLink :to="{ path: '/rules', query: { rule: r.id } }" class="text-ink hover:text-accent">{{
                  r.name || '未命名规则'
                }}</RouterLink>
              </template>
            </template>
            <RouterLink v-else-if="heardByAll(i)" to="/rules" class="hover:text-accent">
              {{ heardByAll(i) }} 条听所有接入的规则在用
            </RouterLink>
            <RouterLink v-else :to="{ path: '/rules', query: { new: '' } }" class="hover:text-accent">
              还没有规则用到，<span class="text-accent">建一条</span>
            </RouterLink>
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
      <p v-if="given.length" class="text-xs text-mute">{{ given.join('、') }}不用接入，建规则时在触发节点里直接填。</p>
    </div>

    <Transition name="drawer">
      <!-- One sheet for both, so that going from one to the other swaps what
           is on it and nothing slides. -->
      <Drawer v-if="picking" :title="picking === 'add' ? '添加接入' : '装上的插件'" @close="picking = false">
        <template v-if="picking === 'add'" #top>
          <div class="flex items-center gap-2 border-b border-line px-5 py-3">
            <div class="relative min-w-0 flex-1">
              <Search :size="14" class="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-mute" />
              <input ref="searchEl" v-model="query" class="input !pl-8" placeholder="搜索名称或用途" />
            </div>
            <div class="seg shrink-0">
              <button v-for="k in kinds" :key="k.id" :class="{ on: kind === k.id }" @click="kind = k.id">
                {{ k.title }}
              </button>
            </div>
          </div>
        </template>
        <template v-if="picking === 'add'">
          <div v-if="foundAddable.length || foundOffered.length" class="list">
            <button
              v-for="p in foundAddable"
              :key="p.id"
              class="row link flex w-full cursor-pointer items-center gap-3 px-4 py-3 text-left"
              @click="add(p)"
            >
              <div class="min-w-0 flex-1">
                <div class="text-sm font-medium">{{ p.name }}</div>
                <p class="mt-0.5 truncate text-xs text-mute" :title="p.description">{{ p.description }}</p>
              </div>
              <ChevronRight :size="16" class="shrink-0 text-faint" />
            </button>
            <!-- From the market: installed first, then connected. -->
            <button
              v-for="p in foundOffered"
              :key="p.id"
              class="row flex w-full items-center gap-3 px-4 py-3 text-left"
              :class="p.supported ? 'link cursor-pointer' : 'cursor-default'"
              :disabled="!p.supported"
              @click="installAndAdd(p)"
            >
              <div class="min-w-0 flex-1" :class="{ 'opacity-55': !p.supported }">
                <div class="flex items-center gap-2 text-sm font-medium">
                  {{ p.name }}<span class="chip">{{ p.version }}</span>
                </div>
                <p class="mt-0.5 truncate text-xs text-mute" :title="p.description">{{ p.description }}</p>
              </div>
              <span v-if="installing === p.id" class="flex shrink-0 items-center gap-1.5 text-xs text-mute tabular-nums"
                ><LoaderCircle :size="14" class="spin" />下载中 {{ progress }}</span
              >
              <span v-else-if="!p.supported" class="shrink-0 text-xs text-mute">不支持当前系统</span>
              <span v-else class="flex shrink-0 items-center gap-1 text-xs text-accent"
                ><Download :size="14" />安装并接入</span
              >
            </button>
          </div>
          <p v-else-if="query.trim() || kind" class="text-sm text-mute">没有找到这样的插件。</p>
          <p v-else class="text-sm text-mute">还没有能接入的插件。</p>
          <p v-if="market.error" class="mt-4 text-xs text-mute">
            这里只有已经装好的插件。插件市场用不了：{{ market.error }}
          </p>
          <button
            v-if="installed.length"
            class="mt-4 cursor-pointer text-xs text-mute hover:text-accent"
            @click="picking = 'plugins'"
          >
            管理装上的插件<template v-if="outdated.length">（{{ outdated.length }} 个可以更新）</template>
          </button>
        </template>
        <template v-else>
          <p class="mb-4 text-xs text-mute">
            从插件市场装上的插件。插件以你的用户身份运行，没有沙箱；安装前会校验文件的 SHA-256。
          </p>
          <div v-if="installed.length" class="list">
            <div v-for="p in installed" :key="p.id" class="row flex min-h-14 items-center gap-4 px-4 py-2.5">
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2 text-sm font-medium">
                  {{ p.name }}<span class="chip">{{ p.installed_version }}</span>
                  <span v-if="p.installed_version !== p.version" class="text-xs font-normal text-ok"
                    >有新版 {{ p.version }}</span
                  >
                </div>
                <p class="truncate text-xs text-mute">{{ p.description }}</p>
              </div>
              <span v-if="installing === p.id" class="flex items-center gap-1.5 text-xs text-mute tabular-nums"
                ><LoaderCircle :size="14" class="spin" />下载中 {{ progress }}</span
              >
              <template v-else>
                <button v-if="p.installed_version !== p.version" class="btn btn-primary" @click="install(p)">
                  更新
                </button>
                <button class="btn btn-ghost btn-danger" @click="uninstall(p)">卸载</button>
              </template>
            </div>
          </div>
          <p v-else class="text-sm text-mute">没有从插件市场装上的插件。</p>
        </template>
        <template v-if="picking === 'plugins'" #footer>
          <button class="btn mr-auto" @click="picking = 'add'">返回添加</button>
        </template>
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
