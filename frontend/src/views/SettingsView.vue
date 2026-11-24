<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { Archive, FolderOpen } from 'lucide-vue-next'
import { useHub } from '../hub'
import { get, post, put } from '../api'
import PageHeader from '../components/PageHeader.vue'
import Drawer from '../components/Drawer.vue'
import McpPanel from '../components/McpPanel.vue'
import BackupDrawer from '../components/BackupDrawer.vue'

const hub = useHub()
const origin = location.origin
const isDesktop = ref(false)
const backupOpen = ref(false)

const publicURL = ref<string | null>(null)
const marketURL = ref<string | null>(null)
const market = ref<{
  configured: boolean
  url?: string
  default_url?: string
  locked?: boolean
}>({ configured: false })

async function loadMarketInfo() {
  const m = await get<{ configured: boolean; url?: string; default_url?: string; locked?: boolean }>('/market').catch(
    () => null,
  )
  if (m) market.value = m
}

async function savePublicURL() {
  const saved = await hub.run(() => put<{ public_url: string }>('/settings', { public_url: publicURL.value }), '已保存')
  if (!saved) return
  hub.publicURL = saved.public_url
  publicURL.value = null
}

async function saveMarketURL() {
  const saved = await hub.run(() => put<{ market_url: string }>('/settings', { market_url: marketURL.value }), '已保存')
  if (!saved) return
  hub.marketURL = saved.market_url
  marketURL.value = null
  await loadMarketInfo()
}

async function openLogs() {
  await hub.run(() => post('/open-logs'))
}

onMounted(async () => {
  loadMarketInfo()
  const st = await get<{ desktop?: boolean }>('/status').catch(() => ({ desktop: false }))
  isDesktop.value = !!st.desktop
})
</script>

<template>
  <div class="flex h-full flex-col">
    <PageHeader title="设置" hint="把 Cat 接入外部 AI 应用，以及对外地址、插件市场、备份迁移与日志" />

    <div class="gutter flex-1 space-y-8 overflow-y-auto pb-10">
      <McpPanel />

      <section>
        <h2 class="mb-3 text-sm font-semibold">网络与插件市场</h2>
        <div class="list">
          <div class="row flex items-center gap-4 px-4 py-3">
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2 text-sm font-medium">
                对外地址
                <span v-if="hub.publicURL" class="chip truncate">{{ hub.publicURL }}</span>
                <span v-else class="chip !font-sans">默认</span>
              </div>
              <p class="mt-0.5 truncate text-xs text-mute">
                <template v-if="hub.publicURL">
                  Webhook 等接入显示和复制的接收地址会以 {{ hub.publicURL }} 开头。
                </template>
                <template v-else>
                  未设置，当前以打开页面的地址（{{ origin }}）开头；使用反向代理或内网穿透时可在此指定。
                </template>
              </p>
            </div>
            <button class="btn shrink-0" @click="publicURL = hub.publicURL">
              {{ hub.publicURL ? '更改' : '设置' }}
            </button>
          </div>

          <div class="row flex items-center gap-4 px-4 py-3">
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2 text-sm font-medium">
                插件市场地址
                <span v-if="market.locked" class="chip !font-sans">已由启动参数锁定</span>
                <span v-else-if="hub.marketURL === 'off'" class="chip !font-sans">已停用</span>
                <span v-else-if="hub.marketURL" class="chip truncate">{{ hub.marketURL }}</span>
                <span v-else class="chip !font-sans">默认</span>
              </div>
              <p class="mt-0.5 truncate text-xs text-mute">
                「添加接入」里浏览和安装扩展插件时读取的索引地址（<code class="font-mono">index.json</code>）。
              </p>
            </div>
            <button v-if="!market.locked" class="btn shrink-0" @click="marketURL = hub.marketURL">
              {{ hub.marketURL ? '更改' : '设置' }}
            </button>
          </div>
        </div>
      </section>

      <section>
        <h2 class="mb-3 text-sm font-semibold">数据与日志</h2>
        <div class="list">
          <div class="row flex items-center gap-4 px-4 py-3">
            <div class="min-w-0 flex-1">
              <div class="text-sm font-medium">备份与迁移</div>
              <p class="mt-0.5 text-xs text-mute">
                导出或导入当前工作区的全局设置、全部接入配置与规则，密钥字段会自动脱敏。
              </p>
            </div>
            <button class="btn shrink-0" @click="backupOpen = true"><Archive :size="14" />备份与迁移</button>
          </div>

          <div v-if="isDesktop" class="row flex items-center gap-4 px-4 py-3">
            <div class="min-w-0 flex-1">
              <div class="text-sm font-medium">本地日志目录</div>
              <p class="mt-0.5 text-xs text-mute">在系统文件管理器中打开 Cat 的本地运行日志文件夹，便于排查问题。</p>
            </div>
            <button class="btn shrink-0" @click="openLogs"><FolderOpen :size="14" />打开日志目录</button>
          </div>
        </div>
      </section>
    </div>

    <Transition name="drawer">
      <Drawer v-if="publicURL !== null" title="对外地址" @close="publicURL = null">
        <p class="mb-4 text-sm text-mute">
          外面的服务访问 Cat 用的地址：反向代理的域名，或内网穿透给的地址。Webhook
          这类接入显示和复制的接收地址会以它开头。
        </p>
        <label class="label">对外地址</label>
        <input
          v-model="publicURL"
          class="input font-mono"
          placeholder="https://cat.example.com"
          @keydown.enter="savePublicURL"
        />
        <p class="mt-2 text-xs text-mute">
          这里只改显示的地址，Cat 监听在哪、谁能访问到它不会变。留空就用打开这个页面的地址（{{ origin }}）。
        </p>
        <template #footer>
          <button class="btn" @click="publicURL = null">取消</button>
          <button class="btn btn-primary" @click="savePublicURL">保存</button>
        </template>
      </Drawer>
    </Transition>

    <Transition name="drawer">
      <Drawer v-if="marketURL !== null" title="插件市场地址" @close="marketURL = null">
        <p class="mb-4 text-sm text-mute">
          「添加接入」里显示和安装未捆绑插件时读取的索引地址（<code class="chip">index.json</code>）。
        </p>
        <label class="label">索引地址</label>
        <input
          v-model="marketURL"
          class="input font-mono"
          :placeholder="market.default_url || 'https://example.com/cat-market/index.json'"
          @keydown.enter="saveMarketURL"
        />
        <p class="mt-2 text-xs text-mute">
          官方插件已随安装包内置；留空或填
          <code class="chip">off</code> 不使用插件市场，填入第三方或自建市场的地址即可在线安装扩展插件。
        </p>
        <template #footer>
          <button class="btn" @click="marketURL = null">取消</button>
          <button class="btn btn-primary" @click="saveMarketURL">保存</button>
        </template>
      </Drawer>
    </Transition>

    <Transition name="drawer">
      <BackupDrawer v-if="backupOpen" @close="backupOpen = false" />
    </Transition>
  </div>
</template>
