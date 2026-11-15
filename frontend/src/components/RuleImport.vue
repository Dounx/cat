<script setup lang="ts">
// Taking in a rule someone exported: paste it or pick the file, choose which
// of the connections here stand in for the ones it was made with (connecting
// what is missing on the spot), and the editor opens on it, unsaved.
import { computed, reactive, ref } from 'vue'
import { ArrowLeft, Check, FileUp, Plus, X } from 'lucide-vue-next'
import { useHub } from '../hub'
import { importRule, needsOf, parseShared, suggest, type Need, type Shared } from '../share'
import type { Rule } from '../types'
import ConnectDialog from './ConnectDialog.vue'

const emit = defineEmits<{ start: [rule: Rule]; close: [] }>()
const hub = useHub()

const text = ref('')
const error = ref('')
const shared = ref<Shared | null>(null)
const chosen = reactive<Record<string, string>>({})
const connecting = ref<Need | null>(null)

const needs = computed(() => (shared.value ? needsOf(shared.value, hub.instances, hub.plugins) : []))
const missing = computed(() => needs.value.filter((n) => !hub.instanceOf(chosen[n.id])))

function read() {
  error.value = ''
  try {
    shared.value = parseShared(text.value)
  } catch (e: any) {
    error.value = e.message
    return
  }
  for (const k of Object.keys(chosen)) delete chosen[k]
  Object.assign(chosen, suggest(needs.value))
  // nothing to choose: straight to the editor
  if (!needs.value.length) start()
}
async function pick(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (!file) return
  text.value = await file.text()
  read()
}
function connected(id: string) {
  chosen[connecting.value!.id] = id
  connecting.value = null
}
function start() {
  emit('start', importRule(shared.value!, chosen))
}
</script>

<template>
  <div class="scrim fixed inset-0 z-40 flex items-start justify-center p-4 pt-[10vh]" @click.self="emit('close')">
    <section class="pop flex max-h-[78vh] w-full max-w-xl flex-col overflow-hidden">
      <header class="flex items-center gap-2 border-b border-line px-4 py-3">
        <button v-if="shared" class="btn btn-ghost px-1.5" aria-label="返回" @click="shared = null">
          <ArrowLeft :size="16" />
        </button>
        <h2 class="mr-auto truncate text-[15px] font-semibold">
          {{ shared ? `导入「${shared.name || '未命名规则'}」` : '导入规则' }}
        </h2>
        <button class="btn btn-ghost px-1.5" aria-label="关闭" @click="emit('close')"><X :size="16" /></button>
      </header>

      <div v-if="!shared" class="flex min-h-0 flex-1 flex-col gap-3 p-4">
        <p class="text-sm text-mute">粘贴别人导出的规则，或选一个 .cat-rule.json 文件。</p>
        <textarea
          v-model="text"
          class="input min-h-40 flex-1 font-mono text-xs"
          placeholder='{ "format": "cat-rule", … }'
          spellcheck="false"
        />
        <p v-if="error" class="rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">{{ error }}</p>
        <div class="flex justify-end gap-2">
          <label class="btn cursor-pointer">
            <FileUp :size="14" />选文件
            <input type="file" accept=".json,application/json" class="hidden" @change="pick" />
          </label>
          <button class="btn btn-primary" :disabled="!text.trim()" @click="read">下一步</button>
        </div>
      </div>

      <div v-else class="flex-1 space-y-4 overflow-y-auto p-5">
        <p class="text-sm text-mute">这条规则用到下面这些接入。选这里的哪一个来顶替，没有的可以现在接入。</p>
        <div v-for="n in needs" :key="n.id">
          <label class="label flex items-center gap-2">
            <span
              class="flex h-4.5 w-4.5 items-center justify-center rounded-full"
              :class="hub.instanceOf(chosen[n.id]) ? 'bg-ok text-white' : 'bg-sunken text-mute'"
            >
              <Check v-if="hub.instanceOf(chosen[n.id])" :size="11" :stroke-width="3" />
            </span>
            {{ n.name }}
            <span class="chip !font-sans">{{ n.installed?.name || n.plugin }}</span>
          </label>
          <div class="flex items-center gap-2">
            <select v-if="n.fits.length" v-model="chosen[n.id]" class="input min-w-0 flex-1">
              <option value="">先不选</option>
              <option v-for="inst in n.fits" :key="inst.id" :value="inst.id">{{ inst.name }}</option>
            </select>
            <span v-else-if="n.installed" class="min-w-0 flex-1 text-sm text-mute"
              >这里还没有接入 {{ n.installed.name }}</span
            >
            <span v-else class="min-w-0 flex-1 text-sm text-warn">
              这里没有装「{{ n.plugin }}」这个插件。到「接入」页点「添加」装上它，或导入后换成别的。
            </span>
            <button v-if="n.installed && !n.installed.singleton" class="btn shrink-0" @click="connecting = n">
              <Plus :size="14" />{{ n.fits.length ? '接入别的' : '现在接入' }}
            </button>
          </div>
        </div>
        <p v-if="missing.length" class="text-xs text-mute">
          还有 {{ missing.length }} 个没选。可以先导入，用到它们的节点会标红，到编辑器里再换。
        </p>
        <div class="flex justify-end pt-1">
          <button class="btn btn-primary" @click="start">下一步：看看这条规则</button>
        </div>
      </div>
    </section>

    <Teleport to="body">
      <Transition name="modal">
        <ConnectDialog
          v-if="connecting?.installed"
          :plugin="connecting.installed"
          ok-text="接入并选用"
          @done="connected"
          @close="connecting = null"
        />
      </Transition>
    </Teleport>
  </div>
</template>
