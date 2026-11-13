<script setup lang="ts">
// The onboarding wizard. The plugin decides the steps; this component only
// renders whatever step the host says is current.
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import QRCode from 'qrcode'
import { CircleCheck, CircleX, ExternalLink, LoaderCircle, X } from 'lucide-vue-next'
import { useHub } from '../hub'
import { get, post } from '../api'
import type { Plugin, Schema } from '../types'
import SchemaForm from './SchemaForm.vue'

interface Step {
  kind: string
  title: string
  description?: string
  text?: string
  image?: string
  url?: string
  progress?: number
  schema?: Schema
  values?: Record<string, any>
  danger?: boolean
  notice?: string
}
interface Session {
  id: string
  plugin: string
  instance_id: string
  state: string
  step?: Step
  error?: string
}

const props = defineProps<{ plugin: Plugin; instanceId?: string }>()
// `rule` asks to go on and build a rule with what was just connected.
const emit = defineEmits<{ close: []; rule: [] }>()
const hub = useHub()

const session = ref<Session | null>(null)
const startError = ref('')
const values = ref<Record<string, any>>({})
const qr = ref('')
const busy = ref(false)
let timer: ReturnType<typeof setInterval> | undefined

const step = computed(() => session.value?.step)
const finished = computed(() => ['done', 'error', 'cancelled'].includes(session.value?.state || ''))
const waiting = computed(() => session.value?.state === 'waiting')

function apply(s: Session) {
  if (!session.value || s.id !== session.value.id) return
  const changed = JSON.stringify(s.step) !== JSON.stringify(session.value.step)
  session.value = s
  if (changed) values.value = { ...(s.step?.values || {}) }
  if (s.state === 'cancelled') emit('close')
}

watch(
  () => hub.signals.setup,
  (s) => s && apply(s as Session),
)
watch(
  step,
  async (s) => {
    qr.value = s?.kind === 'qrcode' && s.text ? await QRCode.toDataURL(s.text, { margin: 2, width: 240 }) : ''
  },
  { immediate: true },
)

// What a step has just done for the user is said in passing, once.
watch(
  () => step.value?.notice,
  (notice) => notice && hub.notify(notice),
)

onMounted(async () => {
  try {
    session.value = await post<Session>('/setup', { plugin: props.plugin.id, instance_id: props.instanceId || '' })
    values.value = { ...(session.value.step?.values || {}) }
  } catch (e: any) {
    startError.value = e.message
    return
  }
  // The live stream normally delivers updates; polling covers any that
  // arrived before the session id was known, or were dropped.
  const refresh = async () => {
    if (session.value && !finished.value)
      apply(await get<Session>(`/setup/${session.value.id}`).catch(() => session.value!))
  }
  refresh()
  timer = setInterval(refresh, 1500)
})
onBeforeUnmount(() => clearInterval(timer))

async function answer(v: Record<string, any>) {
  busy.value = true
  await hub.run(async () => apply(await post<Session>(`/setup/${session.value!.id}/answer`, { values: v })))
  busy.value = false
}
async function cancel() {
  if (session.value && !finished.value)
    await post(`/setup/${session.value.id}/answer`, { cancel: true }).catch(() => {})
  emit('close')
}
</script>

<template>
  <div class="scrim fixed inset-0 z-50 flex items-center justify-center p-4">
    <section class="pop flex max-h-full w-full max-w-md flex-col overflow-hidden">
      <header class="flex items-center justify-between border-b border-line px-5 py-3">
        <h2 class="text-[15px] font-semibold">接入 {{ plugin.name }}</h2>
        <button class="btn btn-ghost px-1.5" aria-label="关闭" @click="cancel"><X :size="16" /></button>
      </header>

      <div class="min-h-40 flex-1 overflow-y-auto p-5">
        <Transition name="swap" mode="out-in">
          <div
            :key="
              startError
                ? 'start'
                : session?.state === 'done' || session?.state === 'error'
                  ? session.state
                  : step
                    ? step.kind
                    : 'boot'
            "
          >
            <p v-if="startError" class="text-sm text-bad">{{ startError }}</p>

            <template v-else-if="session?.state === 'done'">
              <h3 class="mb-1 flex items-center gap-2 text-base font-semibold text-ok">
                <CircleCheck :size="20" class="tick" />接入完成
              </h3>
              <p v-if="step?.kind === 'info'" class="mb-1 text-sm font-medium">{{ step.title }}</p>
              <p class="text-sm whitespace-pre-line text-mute">
                {{ step?.kind === 'info' ? step.description : '已保存并启动。接下来建一条规则，告诉 Cat 用它做什么。' }}
              </p>
            </template>

            <template v-else-if="session?.state === 'error'">
              <h3 class="mb-1 flex items-center gap-2 text-base font-semibold text-bad">
                <CircleX :size="20" class="tick" />接入失败
              </h3>
              <p class="text-sm whitespace-pre-line">{{ session.error }}</p>
            </template>

            <template v-else-if="step">
              <h3 class="mb-1 text-base font-semibold">{{ step.title }}</h3>
              <p v-if="step.description" class="mb-3 text-sm whitespace-pre-line text-mute">{{ step.description }}</p>
              <a v-if="step.url" :href="step.url" target="_blank" rel="noopener" class="btn mb-3"
                ><ExternalLink :size="14" />打开链接</a
              >

              <div v-if="step.kind === 'progress'" class="h-2 overflow-hidden rounded-full bg-sunken">
                <div
                  v-if="(step.progress ?? -1) >= 0"
                  class="h-full rounded-full bg-accent transition-[width] duration-300 ease-glide"
                  :style="{ width: Math.round((step.progress || 0) * 100) + '%' }"
                />
                <div v-else class="bar-sweep h-full w-1/3 rounded-full bg-accent" />
              </div>
              <div v-else-if="step.kind === 'qrcode'" class="flex justify-center">
                <img :src="step.image || qr" alt="二维码" class="h-60 w-60 rounded-xl bg-white p-2" />
              </div>
              <form v-else-if="step.kind === 'form'" @submit.prevent="answer(values)">
                <SchemaForm v-model="values" :schema="step.schema" />
                <button class="btn btn-primary mt-4 w-full" :disabled="busy">继续</button>
              </form>
            </template>

            <p v-else class="flex items-center gap-2 text-sm text-mute">
              <LoaderCircle :size="15" class="spin" />正在启动向导…
            </p>
          </div>
        </Transition>
      </div>

      <footer class="flex justify-end gap-2 border-t border-line px-5 py-3">
        <template v-if="step?.kind === 'confirm' && waiting">
          <button class="btn" :disabled="busy" @click="answer({ confirmed: false })">取消</button>
          <button
            class="btn btn-primary"
            :class="{ '!border-bad !bg-bad': step.danger }"
            :disabled="busy"
            @click="answer({ confirmed: true })"
          >
            确认
          </button>
        </template>
        <template v-else-if="session?.state === 'done' && !instanceId">
          <button class="btn" @click="emit('close')">完成</button>
          <button class="btn btn-primary" @click="emit('rule')">用它建一条规则</button>
        </template>
        <button v-else-if="finished || startError" class="btn btn-primary" @click="emit('close')">完成</button>
        <button v-else class="btn" @click="cancel">取消接入</button>
      </footer>
    </section>
  </div>
</template>
