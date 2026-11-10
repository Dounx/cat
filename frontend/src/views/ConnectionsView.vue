<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { Copy, Pencil, Plus, Power, RefreshCw, Trash2 } from 'lucide-vue-next'
import { useHub } from '../hub'
import { del, get, post, put } from '../api'
import type { Instance, Plugin } from '../types'
import PageHeader from '../components/PageHeader.vue'
import Drawer from '../components/Drawer.vue'
import SchemaForm from '../components/SchemaForm.vue'
import SetupWizard from '../components/SetupWizard.vue'

const hub = useHub()
const editing = ref<null | { plugin: Plugin; id?: string; name: string; enabled: boolean; config: Record<string, any> }>(null)

const addable = computed(() => hub.plugins.filter((p) => !p.singleton))
const configured = computed(() => hub.instances.filter((i) => !hub.pluginOf(i.plugin)?.singleton))
const builtins = computed(() => hub.instances.filter((i) => hub.pluginOf(i.plugin)?.singleton))

const statusText: Record<string, string> = { running: '运行中', starting: '启动中', stopped: '已停用', error: '出错' }
const statusColor: Record<string, string> = { running: 'bg-ok', starting: 'bg-warn', stopped: 'bg-mute', error: 'bg-bad' }

function ingressURL(i: Instance) { return `${location.origin}/ingress/${i.id}` }
async function copy(text: string) {
  await navigator.clipboard.writeText(text)
  hub.notify('已复制')
}

interface MarketPlugin {
  id: string; name: string; version: string; description?: string; homepage?: string
  supported: boolean; installed_version: string; bundled: boolean
}
const market = ref<{ configured: boolean; error?: string; plugins: MarketPlugin[] }>({ configured: false, plugins: [] })
const installing = ref('')
const progress = computed(() => {
  const d = hub.signals.download as { plugin: string; done: number; total: number } | undefined
  if (!d || d.plugin !== installing.value) return ''
  return d.total > 0 ? `${Math.round((d.done / d.total) * 100)}%` : `${(d.done / 1e6).toFixed(1)} MB`
})
async function loadMarket() { market.value = await get('/market').catch(() => market.value) }
onMounted(loadMarket)
watch(() => hub.signals.plugins, () => { hub.reloadPlugins(); loadMarket() })

async function install(p: MarketPlugin) {
  const verb = p.installed_version ? '更新' : '安装'
  if (!confirm(`${verb}插件「${p.name}」${p.version}？\n\n插件以你的用户身份运行，没有沙箱，能做你能做的任何事。请只安装你信任的插件。`)) return
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

function add(p: Plugin) {
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
  if (ok) { editing.value = null; hub.reloadInstances() }
}
async function toggle(i: Instance) {
  await hub.run(() => put(`/instances/${i.id}`, { name: i.name, enabled: !i.enabled, config: i.config }))
  hub.reloadInstances()
}
async function restart(i: Instance) {
  await hub.run(() => post(`/instances/${i.id}/restart`), '已重启')
}
async function remove(i: Instance) {
  if (!confirm(`删除「${i.name}」？引用它的规则将无法执行对应步骤。`)) return
  await hub.run(() => del(`/instances/${i.id}`), '已删除')
  hub.reloadInstances()
}
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="接入" hint="事件从哪里来，动作发到哪里去" />
    <div class="flex-1 space-y-8 overflow-y-auto p-6">
      <section>
        <h2 class="mb-3 text-sm font-semibold">已接入</h2>
        <p v-if="!configured.length" class="card p-6 text-center text-sm text-mute">还没有接入任何来源，从下面选一个开始。</p>
        <div v-for="i in configured" :key="i.id" class="card mb-2 flex items-center gap-4 px-4 py-3">
          <span class="h-2.5 w-2.5 shrink-0 rounded-full" :class="statusColor[i.status]" :title="statusText[i.status]" />
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2 text-sm font-medium">{{ i.name }}<span class="chip">{{ hub.pluginOf(i.plugin)?.name || i.plugin }}</span>
              <span class="text-xs font-normal text-mute">{{ statusText[i.status] }}</span></div>
            <p v-if="i.error" class="mt-0.5 truncate text-xs text-bad" :title="i.error">{{ i.error }}</p>
            <button v-else-if="hub.pluginOf(i.plugin)?.ingress" class="mt-0.5 flex items-center gap-1 font-mono text-xs text-mute hover:text-accent" @click="copy(ingressURL(i))">
              {{ ingressURL(i) }}<Copy :size="12" />
            </button>
          </div>
          <button class="btn btn-ghost px-2" title="编辑" @click="edit(i)"><Pencil :size="15" /></button>
          <button class="btn btn-ghost px-2" title="重启" @click="restart(i)"><RefreshCw :size="15" /></button>
          <button class="btn btn-ghost px-2" :title="i.enabled ? '停用' : '启用'" @click="toggle(i)"><Power :size="15" :class="i.enabled ? 'text-ok' : 'text-mute'" /></button>
          <button class="btn btn-ghost btn-danger px-2" title="删除" @click="remove(i)"><Trash2 :size="15" /></button>
        </div>
      </section>

      <section>
        <h2 class="mb-3 text-sm font-semibold">添加</h2>
        <div class="grid grid-cols-[repeat(auto-fill,minmax(15rem,1fr))] gap-3">
          <button v-for="p in addable" :key="p.id" class="card flex flex-col items-start p-4 text-left transition hover:border-accent" @click="add(p)">
            <div class="mb-1 flex w-full items-center justify-between text-sm font-medium">{{ p.name }}<Plus :size="16" class="text-mute" /></div>
            <p class="mb-2 line-clamp-2 text-xs text-mute">{{ p.description }}</p>
            <div class="mt-auto flex gap-1">
              <span v-if="p.capabilities.includes('source')" class="chip">事件来源</span>
              <span v-if="p.capabilities.includes('action')" class="chip">动作</span>
              <span v-if="!p.builtin" class="chip">插件 {{ p.version }}</span>
            </div>
          </button>
        </div>
      </section>

      <section>
        <h2 class="mb-1 text-sm font-semibold">插件市场</h2>
        <p class="mb-3 text-xs text-mute">插件以你的用户身份运行，没有沙箱。安装前会校验文件的 SHA-256。</p>
        <p v-if="market.error" class="card p-4 text-sm text-mute">{{ market.error }}</p>
        <div v-for="p in market.plugins" :key="p.id" class="card mb-2 flex items-center gap-4 px-4 py-3">
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2 text-sm font-medium">{{ p.name }}<span class="chip">{{ p.version }}</span>
              <span v-if="p.bundled" class="text-xs font-normal text-mute">随应用提供</span>
              <span v-else-if="p.installed_version" class="text-xs font-normal text-ok">已安装 {{ p.installed_version }}</span>
            </div>
            <p class="truncate text-xs text-mute">{{ p.description }}</p>
          </div>
          <span v-if="installing === p.id" class="text-xs text-mute">下载中 {{ progress }}</span>
          <template v-else-if="!p.bundled">
            <span v-if="!p.supported" class="text-xs text-mute">不支持当前系统</span>
            <button v-else-if="!p.installed_version" class="btn" @click="install(p)">安装</button>
            <template v-else>
              <button v-if="p.installed_version !== p.version" class="btn btn-primary" @click="install(p)">更新</button>
              <button class="btn btn-ghost btn-danger" @click="uninstall(p)">卸载</button>
            </template>
          </template>
        </div>
      </section>

      <section>
        <h2 class="mb-3 text-sm font-semibold">内置动作</h2>
        <div class="flex flex-wrap gap-2">
          <div v-for="i in builtins" :key="i.id" class="card flex items-center gap-2 px-3 py-2 text-sm">
            <span class="h-2 w-2 rounded-full" :class="statusColor[i.status]" />{{ i.name }}
            <span class="text-xs text-mute">{{ hub.pluginOf(i.plugin)?.description }}</span>
          </div>
        </div>
      </section>
    </div>

    <Drawer v-if="editing" :title="(editing.id ? '编辑 ' : '添加 ') + editing.plugin.name" @close="editing = null">
      <p class="mb-4 text-sm text-mute">{{ editing.plugin.description }}</p>
      <div class="space-y-3">
        <div><label class="label">名称</label><input v-model="editing.name" class="input" /></div>
        <SchemaForm v-model="editing.config" :schema="editing.plugin.config_schema" />
      </div>
      <template #footer>
        <button v-if="editing.id && editing.plugin.setup" class="btn mr-auto" @click="rerunSetup">重新运行接入向导</button>
        <button class="btn" @click="editing = null">取消</button>
        <button class="btn btn-primary" @click="save">保存</button>
      </template>
    </Drawer>

    <SetupWizard v-if="wizard" :plugin="wizard.plugin" :instance-id="wizard.instanceId" @close="wizard = null; hub.reloadInstances()" />
  </div>
</template>
