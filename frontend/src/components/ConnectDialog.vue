<script setup lang="ts">
// Connect one more thing of a given plugin, wherever that is asked for: the
// plugin's own wizard when it has one, else its config form. Reports which
// connection was made.
import { ref } from 'vue'
import { X } from 'lucide-vue-next'
import { useHub } from '../hub'
import { post } from '../api'
import type { Instance, Plugin } from '../types'
import SchemaForm, { type Picks } from './SchemaForm.vue'
import SetupWizard from './SetupWizard.vue'

const props = defineProps<{ plugin: Plugin; okText?: string; picks?: Picks }>()
const emit = defineEmits<{ done: [instanceId: string]; close: [] }>()
const hub = useHub()

const name = ref(props.plugin.name)
const config = ref<Record<string, any>>({})
// the connections there were, to tell which one a wizard made
const before = hub.instances.map((i) => i.id)

async function create() {
  const made = await hub.run(
    () =>
      post<Instance>('/instances', { plugin: props.plugin.id, name: name.value, enabled: true, config: config.value }),
    `已接入 ${name.value}`,
  )
  if (!made) return
  await hub.reloadInstances()
  emit('done', made.id)
}
// A wizard does not say what it made: it is the one of this plugin that was
// not there before.
async function wizardClosed() {
  await hub.reloadInstances()
  const id = hub.instances.find((i) => i.plugin === props.plugin.id && !before.includes(i.id))?.id
  if (id) emit('done', id)
  else emit('close')
}
</script>

<template>
  <SetupWizard v-if="plugin.setup" :plugin="plugin" @close="wizardClosed" />
  <div v-else class="scrim fixed inset-0 z-50 flex items-center justify-center p-4" @click.self="emit('close')">
    <section class="pop flex max-h-full w-full max-w-md flex-col overflow-hidden">
      <header class="flex items-center justify-between border-b border-line px-5 py-3">
        <h2 class="text-[15px] font-semibold">接入 {{ plugin.name }}</h2>
        <button class="btn btn-ghost px-1.5" aria-label="关闭" @click="emit('close')"><X :size="16" /></button>
      </header>
      <div class="flex-1 space-y-3 overflow-y-auto p-5">
        <p v-if="plugin.description" class="text-sm text-mute">{{ plugin.description }}</p>
        <div><label class="label">名称</label><input v-model="name" class="input" /></div>
        <SchemaForm v-model="config" :schema="plugin.config_schema" :picks="picks" />
      </div>
      <footer class="flex justify-end gap-2 border-t border-line px-5 py-3">
        <button class="btn" @click="emit('close')">取消</button>
        <button class="btn btn-primary" @click="create">{{ okText || '接入' }}</button>
      </footer>
    </section>
  </div>
</template>
