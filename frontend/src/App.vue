<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Activity, ArrowUpCircle, Plug, Settings, Workflow } from 'lucide-vue-next'
import { useHub } from './hub'
import { get, post } from './api'
import type { Update } from './types'

const hub = useHub()
const token = ref('')
const loginError = ref('')
const route = useRoute()
const router = useRouter()

// The thumb of the view switch sits under the view shown and slides to the
// next one. It is placed once without motion, then moves with it.
const navEl = ref<HTMLElement | null>(null)
const thumb = ref({ x: 0, w: 0, moving: false })
function placeThumb() {
  const on = navEl.value?.querySelector<HTMLElement>('a.on')
  if (!on) {
    thumb.value = { x: thumb.value.x, w: 0, moving: false }
    return
  }
  const placed = thumb.value.w > 0
  thumb.value = { x: on.offsetLeft, w: on.offsetWidth, moving: placed }
}
watch(
  () => route.path,
  () => nextTick(placeThumb),
)
// the dot beside 接入 or badge beside 记录 makes its link wider
watch(
  () => [hub.troubled.length > 0, hub.pendingApprovals.length],
  () => nextTick(placeThumb),
)
onBeforeUnmount(() => window.removeEventListener('resize', placeThumb))

const nav = [
  { to: '/rules', label: '规则', icon: Workflow },
  { to: '/connections', label: '接入', icon: Plug },
  { to: '/activity', label: '记录', icon: Activity },
  { to: '/settings', label: '设置', icon: Settings },
]

// The hub looks for a newer version of itself by its own clock, and downloads
// it. Here it is asked to look now, and told to install what it downloaded.
const checking = ref(false)
const downloaded = computed(() => {
  const { done = 0, total = 0 } = hub.update
  return total > 0 ? ` ${Math.floor((done / total) * 100)}%` : ''
})
async function checkUpdate() {
  checking.value = true
  const u = await hub.run(() => post<Update>('/update/check'))
  checking.value = false
  if (!u) return
  hub.update = u
  if (u.state === 'current') hub.notify(`已经是最新版本 ${u.version}`)
  else if (u.state === 'failed') hub.notify(u.reason || '检查更新失败', true)
}
async function applyUpdate() {
  const u = await hub.run(() => post<Update>('/update/apply'))
  if (u) hub.update = u
}

async function login() {
  loginError.value = ''
  try {
    await post('/login', { token: token.value })
    location.reload()
  } catch (e: any) {
    loginError.value = e.message
  }
}

// Inside the desktop shell a webview cannot open new windows, so external
// links are handed to the system browser instead.
onMounted(async () => {
  hub.init()
  window.addEventListener('resize', placeThumb)
  router.isReady().then(() => nextTick(placeThumb))
  document.fonts?.ready.then(placeThumb)
  const status = await get<{ desktop?: boolean }>('/status').catch(() => ({ desktop: false }))
  if (!status.desktop) return
  document.addEventListener('click', (e) => {
    const link = (e.target as HTMLElement).closest?.('a[target="_blank"]') as HTMLAnchorElement | null
    if (!link) return
    e.preventDefault()
    post('/open-url', { url: link.href }).catch((err) => hub.notify(err.message, true))
  })
})
</script>

<template>
  <div class="flex h-full flex-col">
    <header class="gutter grid h-13 shrink-0 grid-cols-[1fr_auto_1fr] items-center gap-4">
      <div class="flex items-center gap-2 justify-self-start">
        <RouterLink to="/rules" class="flex items-center gap-2">
          <img src="/cat.svg" class="h-6 w-6 transition-transform duration-300 ease-glide hover:-rotate-8" alt="" />
          <span class="text-[15px] font-semibold tracking-tight">Cat</span>
        </RouterLink>
        <button
          v-if="hub.update.state !== 'off'"
          class="cursor-pointer pt-0.5 text-xs text-mute tabular-nums hover:text-accent disabled:pointer-events-none"
          title="检查更新"
          :disabled="checking || hub.update.state === 'downloading' || hub.update.state === 'restarting'"
          @click="checkUpdate"
        >
          {{ checking ? '正在检查…' : hub.update.version }}
        </button>
        <span
          v-else-if="hub.update.version && hub.update.version !== 'dev'"
          class="pt-0.5 text-xs text-mute tabular-nums"
        >
          {{ hub.update.version }}
        </span>
      </div>
      <nav ref="navEl" class="seg">
        <span
          class="thumb"
          :class="{ moving: thumb.moving }"
          :style="{ width: thumb.w + 'px', transform: `translateX(${thumb.x}px)` }"
        />
        <RouterLink v-for="n in nav" :key="n.to" :to="n.to" active-class="on">
          <component :is="n.icon" :size="14" />{{ n.label }}
          <!-- seen from every page: a connection has stopped working -->
          <span
            v-if="n.to === '/connections' && hub.troubled.length"
            class="dot bad !h-1.5 !w-1.5"
            :title="`${hub.troubled.length} 个接入出错了`"
          />
          <!-- seen from every page: human-in-the-loop approvals waiting -->
          <span
            v-if="n.to === '/activity' && hub.pendingApprovals.length"
            class="inline-flex h-4 min-w-4 items-center justify-center rounded-full bg-warn px-1 text-[10px] leading-none font-semibold text-white tabular-nums"
            :title="`${hub.pendingApprovals.length} 条待人工确认`"
            >{{ hub.pendingApprovals.length }}</span
          >
        </RouterLink>
      </nav>
      <div class="flex items-center gap-3 justify-self-end text-xs text-mute">
        <!-- a newer version: downloaded and one click away, or to be fetched by hand -->
        <button
          v-if="hub.update.state === 'ready'"
          class="btn btn-primary !rounded-full !px-2.5 !py-1 !text-xs"
          :title="`新版本已经下载好。安装时 Cat 会重新启动，正在执行的规则会中断`"
          @click="applyUpdate"
        >
          <ArrowUpCircle :size="13" />更新到 {{ hub.update.latest }}
        </button>
        <a
          v-else-if="hub.update.state === 'manual'"
          class="flex items-center gap-1 text-accent hover:underline"
          :href="hub.update.notes"
          target="_blank"
          :title="`${hub.update.reason}，需要手动下载安装`"
        >
          <ArrowUpCircle :size="13" />有新版本 {{ hub.update.latest }}
        </a>
        <span v-else-if="hub.update.state === 'downloading'">正在下载 {{ hub.update.latest }}{{ downloaded }}</span>
        <span v-else-if="hub.update.state === 'restarting'">正在重新启动…</span>
        <span
          class="flex items-center gap-2"
          :title="hub.connected ? '收到的事件和规则的执行结果会实时出现' : '正在尝试重新连接'"
        >
          <span class="dot" :class="hub.connected ? 'ok live' : 'bad'" />
          {{ hub.connected ? '实时连接中' : '连接已断开' }}
        </span>
      </div>
    </header>

    <main class="min-h-0 min-w-0 flex-1 overflow-hidden">
      <RouterView v-if="hub.loaded" v-slot="{ Component }">
        <Transition name="view" mode="out-in">
          <component :is="Component" />
        </Transition>
      </RouterView>
    </main>

    <Transition name="toast">
      <div
        v-if="hub.toast"
        class="pop fixed bottom-6 left-1/2 z-50 flex max-w-[80vw] -translate-x-1/2 items-center gap-2 !rounded-full px-4 py-2 text-sm"
      >
        <span class="dot" :class="hub.toast.bad ? 'bad' : 'ok'" />{{ hub.toast.text }}
      </div>
    </Transition>

    <Transition name="modal">
      <div v-if="hub.needAuth" class="scrim fixed inset-0 z-50 flex items-center justify-center">
        <form class="pop w-80 p-5" @submit.prevent="login">
          <h2 class="mb-1 text-base font-semibold">需要访问令牌</h2>
          <p class="mb-3 text-xs text-mute">填写启动 catd 时设置的 --token。</p>
          <input v-model="token" type="password" class="input" autofocus />
          <p v-if="loginError" class="mt-2 text-xs text-bad">{{ loginError }}</p>
          <button class="btn btn-primary mt-3 w-full">进入</button>
        </form>
      </div>
    </Transition>
  </div>
</template>
