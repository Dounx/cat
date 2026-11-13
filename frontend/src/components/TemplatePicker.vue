<script setup lang="ts">
// Starting a rule from what it is for. Pick a template; it lists what it
// needs connected, each chosen from what there is or connected on the spot;
// then the editor opens on the rule it builds.
import { computed, reactive, ref } from 'vue'
import { ArrowLeft, Check, ChevronRight, Plus, X } from 'lucide-vue-next'
import { useHub } from '../hub'
import type { Plugin, Rule } from '../types'
import { templates, type Need, type Template } from '../templates'
import ConnectDialog from './ConnectDialog.vue'

// `inline` shows the choices as part of a page rather than in a dialog.
defineProps<{ inline?: boolean }>()
const emit = defineEmits<{ start: [rule: Rule]; close: [] }>()
const hub = useHub()

const picked = ref<Template | null>(null)
const chosen = reactive<Record<string, string>>({})
const connecting = ref<null | { need: Need; plugin: Plugin }>(null)
const adding = ref('') // the need whose list of plugins to connect is open

const fitting = (n: Need) =>
  hub.instances.filter((i) => {
    const p = hub.pluginOf(i.plugin)
    return !!p && n.fits(p)
  })
const connectable = (n: Need) => hub.plugins.filter((p) => !p.singleton && !p.default_instance && n.fits(p))
const ready = computed(() => !!picked.value?.needs.every((n) => hub.instanceOf(chosen[n.key])))

function pick(t: Template) {
  if (!t.needs.length) return emit('start', t.build({}, hub.plugins))
  for (const k of Object.keys(chosen)) delete chosen[k]
  for (const n of t.needs) chosen[n.key] = fitting(n)[0]?.id || ''
  adding.value = ''
  picked.value = t
}
function connect(need: Need, plugin: Plugin) {
  adding.value = ''
  connecting.value = { need, plugin }
}
function connected(id: string) {
  chosen[connecting.value!.need.key] = id
  connecting.value = null
}
function start() {
  const t = picked.value!
  const got = Object.fromEntries(t.needs.map((n) => [n.key, hub.instanceOf(chosen[n.key])!]))
  emit('start', t.build(got, hub.plugins))
}
</script>

<template>
  <div
    :class="inline ? '' : 'scrim fixed inset-0 z-40 flex items-start justify-center p-4 pt-[10vh]'"
    @click.self="!inline && emit('close')"
  >
    <section :class="inline ? '' : 'pop flex max-h-[78vh] w-full max-w-xl flex-col overflow-hidden'">
      <header
        v-if="!inline || picked"
        class="flex items-center gap-2"
        :class="inline ? 'mb-3' : 'border-b border-line px-4 py-3'"
      >
        <button v-if="picked" class="btn btn-ghost px-1.5" aria-label="返回" @click="picked = null">
          <ArrowLeft :size="16" />
        </button>
        <h2 class="mr-auto text-[15px] font-semibold">{{ picked ? picked.title : '新建规则：你想做什么' }}</h2>
        <button v-if="!inline" class="btn btn-ghost px-1.5" aria-label="关闭" @click="emit('close')">
          <X :size="16" />
        </button>
      </header>

      <div :class="inline ? '' : 'flex-1 overflow-y-auto p-3'">
        <div v-if="!picked" class="grid gap-2.5" :class="inline ? 'sm:grid-cols-2' : ''">
          <button
            v-for="t in templates"
            :key="t.id"
            class="card card-link flex items-center gap-3 px-4 py-3 text-left"
            @click="pick(t)"
          >
            <span class="glyph"><component :is="t.icon" :size="16" /></span>
            <span class="min-w-0 flex-1">
              <span class="block text-sm font-medium">{{ t.title }}</span>
              <span class="block text-xs text-mute">{{ t.about }}</span>
            </span>
            <ChevronRight :size="15" class="shrink-0 text-faint" />
          </button>
        </div>

        <div v-else class="space-y-4" :class="inline ? '' : 'p-2'">
          <p class="text-sm text-mute">{{ picked.about }}。它需要下面这几样，没有的可以现在接入。</p>
          <div v-for="(n, i) in picked.needs" :key="n.key">
            <label class="label flex items-center gap-2">
              <span
                class="flex h-4.5 w-4.5 items-center justify-center rounded-full text-[10.5px] tabular-nums"
                :class="hub.instanceOf(chosen[n.key]) ? 'bg-ok text-white' : 'bg-sunken text-mute'"
              >
                <Check v-if="hub.instanceOf(chosen[n.key])" :size="11" :stroke-width="3" />
                <template v-else>{{ i + 1 }}</template>
              </span>
              {{ n.title }}
            </label>
            <div class="flex items-center gap-2">
              <select v-if="fitting(n).length" v-model="chosen[n.key]" class="input min-w-0 flex-1">
                <option v-for="inst in fitting(n)" :key="inst.id" :value="inst.id">
                  {{ inst.name
                  }}{{ hub.pluginOf(inst.plugin)?.name !== inst.name ? `（${hub.pluginOf(inst.plugin)?.name}）` : '' }}
                </option>
              </select>
              <span v-else class="min-w-0 flex-1 text-sm text-mute">还没有接入</span>
              <button class="btn shrink-0" @click="adding = adding === n.key ? '' : n.key">
                <Plus :size="14" />{{ fitting(n).length ? '接入别的' : '现在接入' }}
              </button>
            </div>
            <div v-if="adding === n.key" class="list mt-2">
              <button
                v-for="p in connectable(n)"
                :key="p.id"
                class="row link flex w-full cursor-pointer items-center gap-3 px-3 py-2 text-left"
                @click="connect(n, p)"
              >
                <span class="min-w-0 flex-1">
                  <span class="block text-sm font-medium">{{ p.name }}</span>
                  <span class="block truncate text-xs text-mute">{{ p.description }}</span>
                </span>
                <ChevronRight :size="15" class="shrink-0 text-faint" />
              </button>
              <p v-if="!connectable(n).length" class="px-3 py-2 text-xs text-mute">
                没有能用的插件，可以到「接入」页点「添加」，从插件市场装一个。
              </p>
            </div>
          </div>
          <div class="flex justify-end pt-1">
            <button class="btn btn-primary" :disabled="!ready" @click="start">下一步：看看这条规则</button>
          </div>
        </div>
      </div>
    </section>

    <Teleport to="body">
      <Transition name="modal">
        <ConnectDialog
          v-if="connecting"
          :plugin="connecting.plugin"
          ok-text="接入并选用"
          @done="connected"
          @close="connecting = null"
        />
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
.glyph {
  display: flex;
  width: 32px;
  height: 32px;
  flex: none;
  align-items: center;
  justify-content: center;
  border-radius: 9px;
  background: var(--color-accent-soft);
  color: var(--color-accent);
}
</style>
