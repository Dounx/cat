<script setup lang="ts">
// A rule as text to hand to someone else: copied, or saved as a file. What
// was typed into its nodes goes along as it is, so it is shown before it
// leaves.
import { computed, ref } from 'vue'
import { Check, Copy, Download, X } from 'lucide-vue-next'
import { useHub } from '../hub'
import { exportRule, fileName } from '../share'
import type { Rule } from '../types'

const props = defineProps<{ rule: Rule }>()
defineEmits<{ close: [] }>()
const hub = useHub()

const text = computed(() => JSON.stringify(exportRule(props.rule, hub.instances), null, 2))
const copied = ref(false)
async function copy() {
  await navigator.clipboard.writeText(text.value)
  copied.value = true
  setTimeout(() => (copied.value = false), 1500)
}
function download() {
  const a = document.createElement('a')
  a.href = URL.createObjectURL(new Blob([text.value], { type: 'application/json' }))
  a.download = fileName(props.rule.name)
  a.click()
  URL.revokeObjectURL(a.href)
}
</script>

<template>
  <div class="scrim fixed inset-0 z-50 flex items-start justify-center p-4 pt-[10vh]" @click.self="$emit('close')">
    <section class="pop flex max-h-[78vh] w-full max-w-xl flex-col overflow-hidden">
      <header class="flex items-center gap-2 border-b border-line px-4 py-3">
        <h2 class="mr-auto text-[15px] font-semibold">导出规则</h2>
        <button class="btn btn-ghost px-1.5" aria-label="关闭" @click="$emit('close')"><X :size="16" /></button>
      </header>
      <div class="flex min-h-0 flex-1 flex-col gap-3 p-4">
        <p class="text-sm text-mute">
          把下面这段发给别人，或存成文件；对方在「规则」页点「导入」，选自己的接入来顶替你的，就得到同一条规则。
        </p>
        <textarea class="input min-h-48 flex-1 font-mono text-xs" readonly :value="text" />
        <p class="text-xs text-warn">
          节点里填的内容会原样带上：地址、请求头里的密钥、会话
          ID、提示词。发出去之前看一眼，该删的先删。接入本身的密钥不在里面。
        </p>
        <div class="flex justify-end gap-2">
          <button class="btn" @click="download"><Download :size="14" />存成文件</button>
          <button class="btn btn-primary" @click="copy">
            <component :is="copied ? Check : Copy" :size="14" />{{ copied ? '已复制' : '复制' }}
          </button>
        </div>
      </div>
    </section>
  </div>
</template>
