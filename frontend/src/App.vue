<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Activity, ListChecks, Plug, Workflow } from 'lucide-vue-next'
import { useHub } from './hub'
import { get, post } from './api'

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
onBeforeUnmount(() => window.removeEventListener('resize', placeThumb))

const nav = [
  { to: '/events', label: '事件', icon: Activity },
  { to: '/rules', label: '规则', icon: Workflow },
  { to: '/runs', label: '运行记录', icon: ListChecks },
  { to: '/connections', label: '接入', icon: Plug },
]

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
      <RouterLink to="/events" class="flex items-center gap-2 justify-self-start">
        <img src="/cat.svg" class="h-6 w-6 transition-transform duration-300 ease-glide hover:-rotate-8" alt="" />
        <span class="text-[15px] font-semibold tracking-tight">Cat</span>
      </RouterLink>
      <nav ref="navEl" class="seg">
        <span
          class="thumb"
          :class="{ moving: thumb.moving }"
          :style="{ width: thumb.w + 'px', transform: `translateX(${thumb.x}px)` }"
        />
        <RouterLink v-for="n in nav" :key="n.to" :to="n.to" active-class="on">
          <component :is="n.icon" :size="14" />{{ n.label }}
        </RouterLink>
      </nav>
      <div
        class="flex items-center gap-2 justify-self-end text-xs text-mute"
        :title="hub.connected ? '事件和运行记录会实时出现' : '正在尝试重新连接'"
      >
        <span class="dot" :class="hub.connected ? 'ok live' : 'bad'" />
        {{ hub.connected ? '实时连接中' : '连接已断开' }}
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
