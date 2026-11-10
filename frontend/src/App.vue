<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { Activity, ListChecks, Plug, Workflow } from 'lucide-vue-next'
import { useHub } from './hub'
import { get, post } from './api'

const hub = useHub()
const token = ref('')
const loginError = ref('')

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
  <div class="flex h-full">
    <aside class="flex w-44 shrink-0 flex-col border-r border-line bg-surface">
      <div class="flex items-center gap-2 px-4 py-4">
        <img src="/cat.svg" class="h-6 w-6" alt="" />
        <span class="text-[15px] font-semibold">Cat</span>
      </div>
      <nav class="flex-1 space-y-0.5 px-2">
        <RouterLink
          v-for="n in nav"
          :key="n.to"
          :to="n.to"
          class="flex items-center gap-2 rounded-md px-2.5 py-2 text-mute hover:bg-sunken hover:text-ink"
          active-class="!bg-accent-soft !text-accent font-medium"
        >
          <component :is="n.icon" :size="16" />{{ n.label }}
        </RouterLink>
      </nav>
      <div class="flex items-center gap-2 px-4 py-3 text-xs text-mute">
        <span class="h-2 w-2 rounded-full" :class="hub.connected ? 'bg-ok' : 'bg-bad'" />
        {{ hub.connected ? '实时连接中' : '连接已断开' }}
      </div>
    </aside>

    <main class="min-w-0 flex-1 overflow-hidden">
      <RouterView v-if="hub.loaded" />
    </main>

    <div
      v-if="hub.toast"
      class="fixed bottom-5 left-1/2 z-50 max-w-[80vw] -translate-x-1/2 rounded-md px-4 py-2 text-sm text-white shadow-lg"
      :class="hub.toast.bad ? 'bg-bad' : 'bg-ink !text-bg'"
    >
      {{ hub.toast.text }}
    </div>

    <div v-if="hub.needAuth" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
      <form class="card w-80 p-5" @submit.prevent="login">
        <h2 class="mb-1 text-base font-semibold">需要访问令牌</h2>
        <p class="mb-3 text-xs text-mute">填写启动 catd 时设置的 --token。</p>
        <input v-model="token" type="password" class="input" autofocus />
        <p v-if="loginError" class="mt-2 text-xs text-bad">{{ loginError }}</p>
        <button class="btn btn-primary mt-3 w-full">进入</button>
      </form>
    </div>
  </div>
</template>
