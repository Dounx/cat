<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  AlertTriangle,
  Check,
  CheckCircle2,
  Copy,
  Download,
  FileUp,
  KeyRound,
  LoaderCircle,
  RefreshCw,
} from 'lucide-vue-next'
import { get, post } from '../api'
import { useHub } from '../hub'
import type { BackupImportResult, WorkspaceBackup } from '../types'
import Drawer from './Drawer.vue'

const emit = defineEmits<{ close: [] }>()
const hub = useHub()

const tab = ref<'export' | 'import'>('export')

// ---- export ----
const backup = ref<WorkspaceBackup | null>(null)
const loadingExport = ref(false)
const copied = ref(false)

const exportedText = computed(() => (backup.value ? JSON.stringify(backup.value, null, 2) : ''))
const maskedInstances = computed(() =>
  (backup.value?.instances || []).filter((i) => (i.masked_secrets?.length || 0) > 0),
)

async function loadBackup() {
  loadingExport.value = true
  try {
    backup.value = await get<WorkspaceBackup>('/backup')
  } catch (e: any) {
    hub.notify(e.message || '加载备份失败', true)
  } finally {
    loadingExport.value = false
  }
}

onMounted(loadBackup)

async function copyBackup() {
  if (!exportedText.value) return
  await navigator.clipboard.writeText(exportedText.value)
  copied.value = true
  setTimeout(() => (copied.value = false), 1500)
}

function downloadBackup() {
  if (!exportedText.value) return
  const day = new Date().toISOString().slice(0, 10)
  const a = document.createElement('a')
  a.href = URL.createObjectURL(new Blob([exportedText.value], { type: 'application/json' }))
  a.download = `cat-workspace-${day}.cat-backup.json`
  a.click()
  URL.revokeObjectURL(a.href)
}

// ---- import ----
const importText = ref('')
const importMode = ref<'merge' | 'replace'>('merge')
const importing = ref(false)
const importError = ref('')
const importResult = ref<BackupImportResult | null>(null)

const parsedPreview = computed(() => {
  const raw = importText.value.trim()
  if (!raw) return null
  try {
    const obj = JSON.parse(raw) as Partial<WorkspaceBackup>
    if (obj.format !== 'cat-backup' || obj.version !== 1) {
      return { valid: false as const, error: '不是有效的 Cat 工作区备份（需要 format: "cat-backup" 且 version: 1）' }
    }
    return {
      valid: true as const,
      backup: obj as WorkspaceBackup,
      instances: Array.isArray(obj.instances) ? obj.instances.length : 0,
      rules: Array.isArray(obj.rules) ? obj.rules.length : 0,
      exportedAt: obj.exported_at ? new Date(obj.exported_at).toLocaleString('zh-CN') : '',
    }
  } catch {
    return { valid: false as const, error: 'JSON 格式有误，请检查粘贴的内容' }
  }
})

async function pickFile(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  importText.value = await file.text()
  importError.value = ''
  importResult.value = null
  input.value = ''
}

async function runImport() {
  importError.value = ''
  importResult.value = null
  const preview = parsedPreview.value
  if (!preview || !preview.valid) {
    importError.value = preview?.error || '请先选择备份文件或粘贴 JSON'
    return
  }
  if (
    importMode.value === 'replace' &&
    !confirm('覆盖模式会清除当前已有的非内置接入和全部规则，并按备份重建（同 ID 接入仍会保留本机已存密钥）。确定继续？')
  ) {
    return
  }
  importing.value = true
  try {
    const res = await post<BackupImportResult>('/backup/import', {
      backup: preview.backup,
      mode: importMode.value,
    })
    importResult.value = res
    await hub.reload()
    await loadBackup()
    hub.notify(`导入完成：接入 +${res.instances_imported}，规则 +${res.rules_imported}`)
  } catch (e: any) {
    importError.value = e.message || '导入失败'
  } finally {
    importing.value = false
  }
}
</script>

<template>
  <Drawer title="备份与迁移" wide @close="emit('close')">
    <template #top>
      <div class="flex items-center justify-between gap-3 border-b border-line px-5 py-3">
        <div class="seg">
          <button :class="{ on: tab === 'export' }" @click="tab = 'export'">导出工作区配置</button>
          <button :class="{ on: tab === 'import' }" @click="tab = 'import'">导入工作区配置</button>
        </div>
        <button v-if="tab === 'export'" class="btn btn-ghost text-xs" :disabled="loadingExport" @click="loadBackup">
          <RefreshCw :size="13" :class="{ spin: loadingExport }" />刷新快照
        </button>
      </div>
    </template>

    <!-- Export tab -->
    <div v-if="tab === 'export'" class="flex h-full flex-col gap-4">
      <p class="text-sm text-mute">
        一次性打包当前工作区的全局设置、全部接入配置与规则，用于备份或迁移到新机器。所有密码与令牌字段会自动脱敏，不会包含明文密钥。
      </p>

      <div v-if="backup" class="grid grid-cols-3 gap-2.5">
        <div class="card px-3.5 py-2.5">
          <div class="text-xs text-mute">接入数量</div>
          <div class="mt-0.5 text-lg font-semibold tabular-nums">{{ backup.instances.length }}</div>
        </div>
        <div class="card px-3.5 py-2.5">
          <div class="text-xs text-mute">规则数量</div>
          <div class="mt-0.5 text-lg font-semibold tabular-nums">{{ backup.rules.length }}</div>
        </div>
        <div class="card px-3.5 py-2.5">
          <div class="text-xs text-mute">对外地址</div>
          <div class="mt-1 truncate font-mono text-xs text-ink" :title="backup.settings.public_url || '未设置'">
            {{ backup.settings.public_url || '未设置' }}
          </div>
        </div>
      </div>

      <div
        v-if="maskedInstances.length"
        class="flex items-start gap-2.5 rounded-xl border border-warn/30 bg-warn/8 px-3.5 py-2.5 text-xs text-warn"
      >
        <KeyRound :size="15" class="mt-0.5 shrink-0" />
        <div class="min-w-0 space-y-1">
          <p class="font-medium">
            以下接入包含已脱敏密钥（<code class="font-mono">••••••••</code>），迁移到新机器后需重新填写：
          </p>
          <div class="flex flex-wrap gap-1.5">
            <span v-for="inst in maskedInstances" :key="inst.id" class="chip !bg-surface/80 !font-sans !text-ink">
              {{ inst.name }}
              <span class="ml-1 font-mono text-[10px] text-mute">({{ inst.masked_secrets?.join(', ') }})</span>
            </span>
          </div>
        </div>
      </div>

      <div class="flex min-h-0 flex-1 flex-col">
        <div class="mb-1.5 flex items-center justify-between">
          <span class="label !mb-0">备份 JSON 预览</span>
          <span v-if="backup?.exported_at" class="text-[11px] text-faint">
            版本 v{{ backup.version }} ·
            {{ new Date(backup.exported_at).toLocaleTimeString('zh-CN', { hour12: false }) }}
          </span>
        </div>
        <textarea
          class="input min-h-64 flex-1 resize-none font-mono text-xs leading-relaxed"
          readonly
          :value="loadingExport && !exportedText ? '正在生成工作区快照…' : exportedText"
        />
      </div>
    </div>

    <!-- Import tab -->
    <div v-else class="space-y-4">
      <p class="text-sm text-mute">
        上传 <code class="chip">.cat-backup.json</code> 文件或粘贴备份 JSON。导入时会保留原接入 ID 与规则
        ID，确保规则引用关系完整不断裂。
      </p>

      <div>
        <label class="label">导入模式</label>
        <div class="grid grid-cols-2 gap-2.5">
          <button
            type="button"
            class="card cursor-pointer px-3.5 py-2.5 text-left transition-colors"
            :class="importMode === 'merge' ? 'border-accent bg-accent-soft/50' : 'hover:bg-sunken/50'"
            @click="importMode = 'merge'"
          >
            <div class="flex items-center justify-between text-sm font-medium">
              <span>合并（保留已有）</span>
              <span class="chip !font-sans">推荐</span>
            </div>
            <p class="mt-1 text-xs text-mute">跳过本机已存在的同 ID 接入与规则，只补充缺失项，不覆盖已有密钥。</p>
          </button>
          <button
            type="button"
            class="card cursor-pointer px-3.5 py-2.5 text-left transition-colors"
            :class="importMode === 'replace' ? 'border-bad/60 bg-bad/5' : 'hover:bg-sunken/50'"
            @click="importMode = 'replace'"
          >
            <div class="flex items-center justify-between text-sm font-medium">
              <span>覆盖</span>
              <span class="chip !font-sans text-bad">替换现有</span>
            </div>
            <p class="mt-1 text-xs text-mute">清除当前非内置接入和全部规则，按备份重建（同机同 ID 仍保留密钥）。</p>
          </button>
        </div>
      </div>

      <div>
        <div class="mb-1.5 flex items-center justify-between">
          <label class="label !mb-0">备份内容</label>
          <label class="btn cursor-pointer !py-1 text-xs">
            <FileUp :size="13" />选择备份文件
            <input type="file" accept=".json,.cat-backup.json,application/json" class="hidden" @change="pickFile" />
          </label>
        </div>
        <textarea
          v-model="importText"
          class="input min-h-44 font-mono text-xs leading-relaxed"
          placeholder='{ "format": "cat-backup", "version": 1, ... }'
          spellcheck="false"
        />
        <div v-if="parsedPreview" class="mt-1.5 text-xs">
          <span v-if="parsedPreview.valid" class="inline-flex items-center gap-1.5 text-ok">
            <CheckCircle2 :size="13" />
            已识别备份：{{ parsedPreview.instances }} 个接入、{{ parsedPreview.rules }} 条规则
            <template v-if="parsedPreview.exportedAt">（导出于 {{ parsedPreview.exportedAt }}）</template>
          </span>
          <span v-else class="inline-flex items-center gap-1.5 text-bad">
            <AlertTriangle :size="13" />{{ parsedPreview.error }}
          </span>
        </div>
      </div>

      <div v-if="importError" class="rounded-xl border border-bad/30 bg-bad/10 px-3.5 py-2.5 text-sm text-bad">
        {{ importError }}
      </div>

      <!-- Structured import summary -->
      <div v-if="importResult" class="card space-y-3 p-4">
        <div class="flex items-center gap-2 text-sm font-semibold text-ok">
          <CheckCircle2 :size="16" />工作区导入完成
          <span v-if="importResult.settings_updated" class="chip !font-sans">全局设置已更新</span>
        </div>
        <div class="grid grid-cols-4 gap-2 text-center">
          <div class="rounded-lg bg-sunken/70 px-2.5 py-2">
            <div class="text-[11px] text-mute">导入接入</div>
            <div class="mt-0.5 text-base font-semibold text-ok tabular-nums">{{ importResult.instances_imported }}</div>
          </div>
          <div class="rounded-lg bg-sunken/70 px-2.5 py-2">
            <div class="text-[11px] text-mute">跳过接入</div>
            <div class="mt-0.5 text-base font-semibold text-mute tabular-nums">
              {{ importResult.instances_skipped }}
            </div>
          </div>
          <div class="rounded-lg bg-sunken/70 px-2.5 py-2">
            <div class="text-[11px] text-mute">导入规则</div>
            <div class="mt-0.5 text-base font-semibold text-ok tabular-nums">{{ importResult.rules_imported }}</div>
          </div>
          <div class="rounded-lg bg-sunken/70 px-2.5 py-2">
            <div class="text-[11px] text-mute">跳过规则</div>
            <div class="mt-0.5 text-base font-semibold text-mute tabular-nums">{{ importResult.rules_skipped }}</div>
          </div>
        </div>

        <div
          v-if="importResult.needs_secrets.length"
          class="rounded-xl border border-warn/35 bg-warn/10 px-3.5 py-2.5 text-xs text-warn"
        >
          <div class="flex items-center gap-1.5 font-medium">
            <KeyRound :size="14" />以下接入需要重新填写密钥（备份中已脱敏）：
          </div>
          <ul class="mt-1.5 list-disc space-y-0.5 pl-5 text-ink">
            <li v-for="name in importResult.needs_secrets" :key="name" class="font-medium">{{ name }}</li>
          </ul>
          <p class="mt-1.5 text-[11px] text-mute">请在「接入」列表点击对应接入的「编辑」按钮补填密钥并保存。</p>
        </div>

        <div
          v-if="importResult.warnings.length"
          class="rounded-xl border border-line bg-sunken/50 px-3.5 py-2.5 text-xs text-mute"
        >
          <div class="flex items-center gap-1.5 font-medium text-ink">
            <AlertTriangle :size="14" class="text-warn" />注意事项（{{ importResult.warnings.length }}）
          </div>
          <ul class="mt-1.5 list-disc space-y-0.5 pl-5">
            <li v-for="(w, idx) in importResult.warnings" :key="idx">{{ w }}</li>
          </ul>
        </div>
      </div>
    </div>

    <template #footer>
      <template v-if="tab === 'export'">
        <button class="btn" :disabled="!exportedText" @click="copyBackup">
          <component :is="copied ? Check : Copy" :size="14" />{{ copied ? '已复制' : '复制 JSON' }}
        </button>
        <button class="btn btn-primary" :disabled="!exportedText" @click="downloadBackup">
          <Download :size="14" />导出工作区配置
        </button>
      </template>
      <template v-else>
        <button class="btn" @click="emit('close')">关闭</button>
        <button class="btn btn-primary" :disabled="importing || !parsedPreview?.valid" @click="runImport">
          <LoaderCircle v-if="importing" :size="14" class="spin" />
          {{ importing ? '导入中…' : '导入工作区配置' }}
        </button>
      </template>
    </template>
  </Drawer>
</template>
