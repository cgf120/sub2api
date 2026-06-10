<template>
  <BaseDialog
    :show="show"
    title="批量导入 GPT/Grok/Gemini/Anthropic 账号"
    width="wide"
    :close-on-click-outside="false"
    @close="handleClose"
  >
    <div class="space-y-5">
      <div class="rounded-lg border border-gray-200 bg-gray-50 px-4 py-3 dark:border-dark-600 dark:bg-dark-700/40">
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <div class="text-sm font-medium text-gray-900 dark:text-white">Excel 模板</div>
            <div class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              A 列名称可空，B 列类型填 gpt、grok、gemini 或 anthropic，C 列填 SK 或 SSO。每个账号一行。
            </div>
          </div>
          <button type="button" class="btn btn-secondary" @click="downloadTemplate">
            <Icon name="download" size="sm" />
            下载模板
          </button>
        </div>
      </div>

      <div class="rounded-lg border border-gray-200 px-4 py-3 dark:border-dark-600">
        <div class="mb-3 text-sm font-medium text-gray-900 dark:text-white">导入设置</div>
        <div class="grid gap-4 md:grid-cols-2">
          <ProxySelector v-model="selectedProxyId" :proxies="props.proxies" :disabled="isRunning" />
          <GroupSelector v-model="selectedGroupIds" :groups="props.groups" searchable />
        </div>
      </div>

      <div>
        <input
          ref="fileInputRef"
          type="file"
          accept=".xlsx,.xls"
          class="hidden"
          @change="handleFileChange"
        />
        <button
          type="button"
          class="flex w-full items-center justify-center gap-2 rounded-lg border-2 border-dashed border-gray-300 px-4 py-8 text-sm text-gray-600 transition-colors hover:border-primary-400 hover:bg-primary-50 hover:text-primary-700 dark:border-dark-500 dark:text-gray-300 dark:hover:border-primary-600 dark:hover:bg-primary-900/20 dark:hover:text-primary-300"
          :disabled="isRunning"
          @click="fileInputRef?.click()"
        >
          <Icon name="upload" size="md" />
          <span>{{ selectedFileName || '选择 Excel 文件' }}</span>
        </button>
      </div>

      <div v-if="parsedRows.length > 0" class="rounded-lg border border-gray-200 dark:border-dark-600">
        <div class="flex items-center justify-between border-b border-gray-200 px-4 py-3 dark:border-dark-600">
          <div>
            <div class="text-sm font-medium text-gray-900 dark:text-white">解析结果</div>
            <div class="text-xs text-gray-500 dark:text-gray-400">
              {{ parsedRows.length }} 行，{{ validRowCount }} 行可导入
            </div>
          </div>
          <span
            v-if="parseErrors.length > 0"
            class="rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
          >
            {{ parseErrors.length }} 个提示
          </span>
        </div>
        <div v-if="parseErrors.length > 0" class="max-h-28 overflow-y-auto px-4 py-3 text-xs text-amber-700 dark:text-amber-300">
          <div v-for="error in parseErrors.slice(0, 8)" :key="error">{{ error }}</div>
          <div v-if="parseErrors.length > 8">还有 {{ parseErrors.length - 8 }} 个提示会在导入结果中显示。</div>
        </div>
      </div>

      <div v-if="job" class="space-y-3 rounded-lg border border-gray-200 px-4 py-3 dark:border-dark-600">
        <div class="flex items-center justify-between text-sm">
          <div class="font-medium text-gray-900 dark:text-white">导入进度</div>
          <div class="text-gray-500 dark:text-gray-400">{{ job.processed }} / {{ job.total }}</div>
        </div>
        <div class="h-2 overflow-hidden rounded-full bg-gray-100 dark:bg-dark-700">
          <div class="h-full rounded-full bg-primary-500 transition-all" :style="{ width: `${progressPercent}%` }"></div>
        </div>
        <div class="flex flex-wrap gap-3 text-xs text-gray-500 dark:text-gray-400">
          <span>状态：{{ statusLabel }}</span>
          <span>成功：{{ job.success }}</span>
          <span>失败：{{ job.failed }}</span>
          <span v-if="warningResults.length > 0">提示：{{ warningResults.length }}</span>
        </div>
        <div v-if="job.error" class="rounded bg-rose-50 px-3 py-2 text-xs text-rose-700 dark:bg-rose-900/20 dark:text-rose-300">
          {{ job.error }}
        </div>
      </div>

      <div v-if="warningResults.length > 0" class="rounded-lg border border-gray-200 dark:border-dark-600">
        <div class="border-b border-gray-200 px-4 py-3 text-sm font-medium text-gray-900 dark:border-dark-600 dark:text-white">
          提示明细
        </div>
        <div class="max-h-48 overflow-y-auto divide-y divide-gray-100 dark:divide-dark-700">
          <div
            v-for="item in warningResults.slice(0, 20)"
            :key="`${item.row_number}-${item.warning}`"
            class="grid grid-cols-[5rem_1fr] gap-3 px-4 py-2 text-xs"
          >
            <span class="text-gray-500 dark:text-gray-400">第 {{ item.row_number }} 行</span>
            <span class="text-amber-700 dark:text-amber-300">{{ item.warning }}</span>
          </div>
        </div>
      </div>

      <div v-if="failedResults.length > 0" class="rounded-lg border border-gray-200 dark:border-dark-600">
        <div class="border-b border-gray-200 px-4 py-3 text-sm font-medium text-gray-900 dark:border-dark-600 dark:text-white">
          失败明细
        </div>
        <div class="max-h-48 overflow-y-auto divide-y divide-gray-100 dark:divide-dark-700">
          <div
            v-for="item in failedResults.slice(0, 20)"
            :key="`${item.row_number}-${item.error}`"
            class="grid grid-cols-[5rem_1fr] gap-3 px-4 py-2 text-xs"
          >
            <span class="text-gray-500 dark:text-gray-400">第 {{ item.row_number }} 行</span>
            <span class="text-rose-600 dark:text-rose-300">{{ item.error || '导入失败' }}</span>
          </div>
        </div>
      </div>
    </div>

    <template #footer>
      <button type="button" class="btn btn-secondary" :disabled="isRunning" @click="handleClose">关闭</button>
      <button
        type="button"
        class="btn btn-primary"
        :disabled="!canStartImport"
        @click="startImport"
      >
        <Icon v-if="isRunning" name="refresh" size="sm" class="animate-spin" />
        {{ isRunning ? '导入中' : '开始导入' }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import * as XLSX from 'xlsx'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import type { AccountBulkImportItem, AccountBulkImportJob, AdminGroup, Proxy as AccountProxy } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import GroupSelector from '@/components/common/GroupSelector.vue'
import ProxySelector from '@/components/common/ProxySelector.vue'
import Icon from '@/components/icons/Icon.vue'

const props = withDefaults(defineProps<{
  show: boolean
  proxies?: AccountProxy[]
  groups?: AdminGroup[]
}>(), {
  proxies: () => [],
  groups: () => []
})
const emit = defineEmits<{
  close: []
  imported: []
}>()

const appStore = useAppStore()
const fileInputRef = ref<HTMLInputElement | null>(null)
const selectedFileName = ref('')
const parsedRows = ref<AccountBulkImportItem[]>([])
const parseErrors = ref<string[]>([])
const job = ref<AccountBulkImportJob | null>(null)
const selectedProxyId = ref<number | null>(null)
const selectedGroupIds = ref<number[]>([])
const completionNotified = ref(false)
let pollTimer: ReturnType<typeof setInterval> | null = null

const allowedTypes = new Set([
  'gpt',
  'openai',
  'chatgpt',
  'grok',
  'xai',
  'x.ai',
  'gemini',
  'google',
  'google-ai',
  'google_ai',
  'ai-studio',
  'ai_studio',
  'aistudio',
  'anthropic',
  'claude',
  'claude-api',
  'claude_api'
])
const isRunning = computed(() => job.value?.status === 'pending' || job.value?.status === 'running')
const validRowCount = computed(() =>
  parsedRows.value.filter((row) => allowedTypes.has(row.type.trim().toLowerCase()) && row.credential.trim()).length
)
const canStartImport = computed(() => !isRunning.value && parsedRows.value.length > 0 && validRowCount.value > 0)
const progressPercent = computed(() => {
  if (!job.value || job.value.total <= 0) return 0
  return Math.min(100, Math.round((job.value.processed / job.value.total) * 100))
})
const statusLabel = computed(() => {
  switch (job.value?.status) {
    case 'pending':
      return '等待中'
    case 'running':
      return '进行中'
    case 'completed':
      return '已完成'
    case 'failed':
      return '任务失败'
    default:
      return '-'
  }
})
const failedResults = computed(() => job.value?.results.filter((item) => !item.success) || [])
const warningResults = computed(() => job.value?.results.filter((item) => item.success && item.warning) || [])

const normalizeCell = (value: unknown) => String(value ?? '').trim()

const hasHeaderRow = (row: unknown[]) => {
  const text = row.map(normalizeCell).join('|').toLowerCase()
  return text.includes('名称') || text.includes('name') || text.includes('类型') || text.includes('type') || text.includes('sso')
}

const downloadTemplate = () => {
  const workbook = XLSX.utils.book_new()
  const importSheet = XLSX.utils.aoa_to_sheet([
    ['名称（非必填）', '类型（gpt/grok/gemini/anthropic）', 'SK 或 SSO']
  ])
  importSheet['!cols'] = [{ wch: 28 }, { wch: 18 }, { wch: 90 }]
  XLSX.utils.book_append_sheet(workbook, importSheet, '导入')

  const exampleSheet = XLSX.utils.aoa_to_sheet([
    ['名称（非必填）', '类型（gpt/grok/gemini/anthropic）', 'SK 或 SSO'],
    ['', 'gpt', 'sk-proj-...'],
    ['grok1', 'grok', 'eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9...'],
    ['gemini1', 'gemini', 'AIza...'],
    ['anthropic1', 'anthropic', 'sk-ant-...']
  ])
  exampleSheet['!cols'] = [{ wch: 28 }, { wch: 18 }, { wch: 90 }]
  XLSX.utils.book_append_sheet(workbook, exampleSheet, '示例')
  XLSX.writeFile(workbook, 'account-bulk-import-template.xlsx')
}

const handleFileChange = async (event: Event) => {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  selectedFileName.value = file.name
  job.value = null
  completionNotified.value = false
  stopPolling()

  try {
    const workbook = XLSX.read(await file.arrayBuffer(), { type: 'array' })
    const sheetName = workbook.SheetNames.includes('导入') ? '导入' : workbook.SheetNames[0]
    if (!sheetName) {
      throw new Error('Excel 文件没有工作表')
    }
    const sheet = workbook.Sheets[sheetName]
    const matrix = XLSX.utils.sheet_to_json<unknown[]>(sheet, { header: 1, defval: '', blankrows: false })
    parseSheetRows(matrix)
  } catch (error: any) {
    parsedRows.value = []
    parseErrors.value = []
    appStore.showError(error?.message || 'Excel 解析失败')
  }
}

const parseSheetRows = (matrix: unknown[][]) => {
  const startIndex = matrix.length > 0 && hasHeaderRow(matrix[0]) ? 1 : 0
  const rows: AccountBulkImportItem[] = []
  const errors: string[] = []

  for (let index = startIndex; index < matrix.length; index++) {
    const row = matrix[index] || []
    const name = normalizeCell(row[0])
    const type = normalizeCell(row[1]).toLowerCase()
    const credential = normalizeCell(row[2])
    if (!name && !type && !credential) continue

    const rowNumber = index + 1
    rows.push({ row_number: rowNumber, name, type, credential })
    if (!type) {
      errors.push(`第 ${rowNumber} 行缺少类型`)
    } else if (!allowedTypes.has(type)) {
      errors.push(`第 ${rowNumber} 行类型不是 gpt、grok、gemini 或 anthropic`)
    }
    if (!credential) {
      errors.push(`第 ${rowNumber} 行缺少 SK/SSO`)
    }
  }

  parsedRows.value = rows
  parseErrors.value = errors
  if (rows.length === 0) {
    appStore.showError('没有解析到可导入的账号')
  }
}

const startImport = async () => {
  if (!canStartImport.value) return
  try {
    completionNotified.value = false
    job.value = await adminAPI.accounts.startBulkImport(parsedRows.value, {
      proxy_id: selectedProxyId.value,
      group_ids: selectedGroupIds.value
    })
    startPolling()
  } catch (error: any) {
    appStore.showError(error.response?.data?.message || error.message || '启动导入失败')
  }
}

const startPolling = () => {
  stopPolling()
  pollTimer = setInterval(pollJob, 1000)
  void pollJob()
}

const stopPolling = () => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

const pollJob = async () => {
  if (!job.value?.id) return
  try {
    job.value = await adminAPI.accounts.getBulkImportJob(job.value.id)
    if (job.value.status === 'completed' || job.value.status === 'failed') {
      stopPolling()
      notifyCompletion()
    }
  } catch (error: any) {
    stopPolling()
    appStore.showError(error.response?.data?.message || error.message || '获取导入进度失败')
  }
}

const notifyCompletion = () => {
  if (!job.value || completionNotified.value) return
  completionNotified.value = true
  if (job.value.success > 0) {
    emit('imported')
  }
  if (job.value.failed > 0 || warningResults.value.length > 0) {
    appStore.showWarning(`导入完成：成功 ${job.value.success}，失败 ${job.value.failed}，提示 ${warningResults.value.length}`)
  } else {
    appStore.showSuccess(`导入完成：成功 ${job.value.success}`)
  }
}

const resetState = () => {
  selectedFileName.value = ''
  parsedRows.value = []
  parseErrors.value = []
  job.value = null
  selectedProxyId.value = null
  selectedGroupIds.value = []
  completionNotified.value = false
}

const handleClose = () => {
  if (isRunning.value) {
    appStore.showWarning('导入进行中，请等待完成')
    return
  }
  stopPolling()
  resetState()
  emit('close')
}

watch(
  () => props.show,
  (visible) => {
    if (!visible && !isRunning.value) {
      stopPolling()
      resetState()
    }
  }
)

onUnmounted(stopPolling)
</script>
