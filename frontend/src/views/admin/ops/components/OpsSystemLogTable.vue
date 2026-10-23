<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useMediaQuery } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import LogRetentionSelect from './LogRetentionSelect.vue'
import { opsAPI, type OpsRuntimeLogConfig, type OpsSystemLog, type OpsSystemLogSinkHealth, type OpsSystemLogQuery } from '@/api/admin/ops'
import RequestDiagnosticDetails from './RequestDiagnosticDetails.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import { useAppStore } from '@/stores'
import { extractApiErrorMessage } from '@/utils/apiError'

const appStore = useAppStore()
const { t } = useI18n()

// 与 DataTable 一致：< 768px 切换为卡片视图，避免宽表在移动端被截断。
const isDesktopViewport = useMediaQuery('(min-width: 768px)')

const props = withDefaults(defineProps<{
  platformFilter?: string
  refreshToken?: number
  diagnosticsMode?: boolean
}>(), {
  platformFilter: '',
  refreshToken: 0,
  diagnosticsMode: false
})

const loading = ref(false)
const logs = ref<OpsSystemLog[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

const health = ref<OpsSystemLogSinkHealth>({
  queue_depth: 0,
  queue_capacity: 0,
  dropped_count: 0,
  write_failed_count: 0,
  written_count: 0,
  avg_write_delay_ms: 0
})

const runtimeLoading = ref(false)
const runtimeConfigLoaded = ref(false)
const runtimeSaving = ref(false)
const runtimeConfig = reactive<OpsRuntimeLogConfig & { diagnostic_retention_days: number; diagnostic_max_size_mb: number }>({
  level: 'info',
  persist_access_logs: false,
  persist_request_diagnostics: false,
  enable_sampling: false,
  sampling_initial: 100,
  sampling_thereafter: 100,
  caller: true,
  stacktrace_level: 'error',
  retention_days: 30,
  request_retention_days: 90,
  diagnostic_retention_days: 7,
  diagnostic_max_size_mb: 0
})
const savedDiagnosticPolicy = ref({ days: 7, maxSizeMB: 0 })
function loadDiagnosticPolicy(cfg: OpsRuntimeLogConfig) {
  runtimeConfig.diagnostic_retention_days = cfg.diagnostic_retention_days ?? 7
  runtimeConfig.diagnostic_max_size_mb = cfg.diagnostic_max_size_mb ?? 0
  savedDiagnosticPolicy.value = { days: runtimeConfig.diagnostic_retention_days, maxSizeMB: runtimeConfig.diagnostic_max_size_mb }
}

const filters = reactive({
  time_range: '1h' as '5m' | '30m' | '1h' | '6h' | '24h' | '7d' | '30d',
  start_time: '',
  end_time: '',
  host: '',
  level: '',
  component: '',
  request_id: '',
  client_request_id: '',
  user_id: '',
  api_key_id: '',
  account_id: '',
  platform: '',
  model: '',
  q: ''
})

const runtimeLevelOptions = [
  { value: 'debug', label: 'debug' },
  { value: 'info', label: 'info' },
  { value: 'warn', label: 'warn' },
  { value: 'error', label: 'error' }
]

const stacktraceLevelOptions = [
  { value: 'none', label: 'none' },
  { value: 'error', label: 'error' },
  { value: 'fatal', label: 'fatal' }
]

const timeRangeOptions = [
  { value: '5m', label: '5m' },
  { value: '30m', label: '30m' },
  { value: '1h', label: '1h' },
  { value: '6h', label: '6h' },
  { value: '24h', label: '24h' },
  { value: '7d', label: '7d' },
  { value: '30d', label: '30d' }
]

const filterLevelOptions = computed(() => [
  { value: '', label: t('admin.ops.systemLogs.all') },
  { value: 'debug', label: 'debug' },
  { value: 'info', label: 'info' },
  { value: 'warn', label: 'warn' },
  { value: 'error', label: 'error' }
])

const levelBadgeClass = (level: string) => {
  const v = String(level || '').toLowerCase()
  if (v === 'error' || v === 'fatal') return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  if (v === 'warn' || v === 'warning') return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  if (v === 'debug') return 'bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300'
  return 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
}

const formatTime = (value: string) => {
  if (!value) return '-'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return d.toLocaleString()
}

const getExtraString = (extra: Record<string, any> | undefined, key: string) => {
  if (!extra) return ''
  const v = extra[key]
  if (v == null) return ''
  if (typeof v === 'string') return v.trim()
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  return ''
}

const formatSystemLogDetail = (row: OpsSystemLog) => {
  const parts: string[] = []
  const msg = String(row.message || '').trim()
  if (msg) parts.push(msg)

  const extra = row.extra || {}
  const statusCode = getExtraString(extra, 'status_code')
  const latencyMs = getExtraString(extra, 'latency_ms')
  const method = getExtraString(extra, 'method')
  const path = getExtraString(extra, 'path')
  const clientIP = getExtraString(extra, 'client_ip')
  const protocol = getExtraString(extra, 'protocol')

  const accessParts: string[] = []
  if (statusCode) accessParts.push(`status=${statusCode}`)
  if (latencyMs) accessParts.push(`latency_ms=${latencyMs}`)
  if (method) accessParts.push(`method=${method}`)
  if (path) accessParts.push(`path=${path}`)
  if (clientIP) accessParts.push(`ip=${clientIP}`)
  if (protocol) accessParts.push(`proto=${protocol}`)
  if (accessParts.length > 0) parts.push(accessParts.join(' '))

  const corrParts: string[] = []
  if (row.request_id) corrParts.push(`req=${row.request_id}`)
  if (row.client_request_id) corrParts.push(`client_req=${row.client_request_id}`)
  if (row.user_id != null) corrParts.push(`user=${row.user_id}`)
  if (row.api_key_id != null) corrParts.push(`key=${row.api_key_id}`)
  if (row.account_id != null) corrParts.push(`acc=${row.account_id}`)
  if (row.platform) corrParts.push(`platform=${row.platform}`)
  if (row.model) corrParts.push(`model=${row.model}`)
  if (corrParts.length > 0) parts.push(corrParts.join(' '))

  const errors = getExtraString(extra, 'errors')
  if (errors) parts.push(`errors=${errors}`)
  const err = getExtraString(extra, 'err') || getExtraString(extra, 'error')
  if (err) parts.push(`error=${err}`)

  // 用空格拼接，交给 CSS 自动换行，尽量“填满再换行”。
  return parts.join('  ')
}

const toRFC3339 = (value: string) => {
  if (!value) return undefined
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return undefined
  return d.toISOString()
}

const buildQuery = () => {
  const query: OpsSystemLogQuery = {
    page: page.value,
    page_size: pageSize.value,
    time_range: filters.time_range
  }

  if (filters.time_range === '30d') {
    query.time_range = '30d'
  }
  if (filters.start_time) query.start_time = toRFC3339(filters.start_time)
  if (filters.end_time) query.end_time = toRFC3339(filters.end_time)
  if (filters.host.trim()) query.host = filters.host.trim()
  if (filters.level.trim()) query.level = filters.level.trim()
  if (filters.component.trim()) query.component = filters.component.trim()
  if (filters.request_id.trim()) query.request_id = filters.request_id.trim()
  if (filters.client_request_id.trim()) query.client_request_id = filters.client_request_id.trim()
  if (filters.user_id.trim()) {
    const v = Number.parseInt(filters.user_id.trim(), 10)
    if (Number.isFinite(v) && v > 0) query.user_id = v
  }
  if (filters.api_key_id.trim()) {
    const v = Number.parseInt(filters.api_key_id.trim(), 10)
    if (Number.isFinite(v) && v > 0) query.api_key_id = v
  }
  if (filters.account_id.trim()) {
    const v = Number.parseInt(filters.account_id.trim(), 10)
    if (Number.isFinite(v) && v > 0) query.account_id = v
  }
  if (filters.platform.trim()) query.platform = filters.platform.trim()
  if (filters.model.trim()) query.model = filters.model.trim()
  if (filters.q.trim()) query.q = filters.q.trim()
  if (preset.value === 'diagnostics') query.event = DIAGNOSTIC_EVENT
  if (preset.value === 'access') query.component = 'http.access'
  return query
}

const DIAGNOSTIC_EVENT = 'openai.messages_ws_latency'
type LogPreset = 'all' | 'diagnostics' | 'access'
const preset = ref<LogPreset>(props.diagnosticsMode ? 'diagnostics' : 'all')
const presetOptions = computed(() => [
  { value: 'all', label: t('admin.ops.systemLogs.presetAll') },
  { value: 'diagnostics', label: t('admin.ops.systemLogs.presetDiagnostics') },
  { value: 'access', label: t('admin.ops.systemLogs.presetAccess') }
])
const isDiagnostic = (row: OpsSystemLog) => row.extra?.event === DIAGNOSTIC_EVENT || row.message === DIAGNOSTIC_EVENT
const selectedLog = ref<OpsSystemLog | null>(null)
const liveEnabled = ref(props.diagnosticsMode)
const manuallyPaused = ref(false)
const visible = ref(document.visibilityState !== 'hidden')
const loadError = ref(false)
const opsDisabled = ref(false)
const failureCount = ref(0)
const lastUpdated = ref('')
const appliedQuery = ref<OpsSystemLogQuery>({})
type TimeBounds = { start_time: string; end_time: string }
let displayedBounds: TimeBounds | null = null
let frozenBounds: TimeBounds | null = null
let pollTimer: ReturnType<typeof setTimeout> | undefined
let generation = 0
let inFlight = false
let pending = false
let disposed = false
const canPoll = () => liveEnabled.value && visible.value && page.value === 1 && !selectedLog.value && !opsDisabled.value && !disposed
const liveStatus = computed(() => {
  if (opsDisabled.value) return 'monitoringDisabled'
  if (!liveEnabled.value) return 'livePaused'
  if (!visible.value) return 'liveHidden'
  if (selectedLog.value) return 'liveDetails'
  if (page.value !== 1) return 'liveOlderPage'
  return failureCount.value ? 'liveRetrying' : 'liveActive'
})

function clearPoll() {
  if (pollTimer !== undefined) clearTimeout(pollTimer)
  pollTimer = undefined
}
function invalidate() {
  clearPoll()
  generation++
  pending = false
  loading.value = false
}
function boundsForQuery(): TimeBounds {
  if (frozenBounds) return frozenBounds
  const query = appliedQuery.value
  const end = query.end_time ? new Date(query.end_time).getTime() : Date.now()
  const ranges = { '5m': 300000, '30m': 1800000, '1h': 3600000, '6h': 21600000, '24h': 86400000, '7d': 604800000, '30d': 2592000000 }
  return {
    start_time: query.start_time || new Date(end - ranges[query.time_range || '1h']).toISOString(),
    end_time: new Date(end).toISOString()
  }
}
function freeze() {
  frozenBounds = displayedBounds || boundsForQuery()
  invalidate()
}
function schedulePoll() {
  clearPoll()
  if (canPoll()) pollTimer = setTimeout(() => { void fetchLogs() }, Math.min(30000, 2000 * 2 ** Math.min(failureCount.value, 4)))
}
function handleDisabled(err: unknown): boolean {
  if (typeof err === 'object' && err !== null && 'code' in err && err.code === 'OPS_DISABLED') {
    opsDisabled.value = true
    invalidate()
    return true
  }
  return false
}

// One worker for all refresh sources: serialize requests and reject stale generations.
const fetchLogs = async () => {
  if (disposed || opsDisabled.value) return
  clearPoll()
  generation++
  pending = true
  if (inFlight) return
  inFlight = true
  try {
    while (pending && !disposed && !opsDisabled.value) {
      pending = false
      const ownGeneration = generation
      const bounds = boundsForQuery()
      const query = { ...appliedQuery.value, ...bounds, page: page.value, page_size: pageSize.value }
      loading.value = logs.value.length === 0
      try {
        const res = await opsAPI.listSystemLogs(query)
        if (disposed || ownGeneration !== generation) continue
        logs.value = res.items || []
        total.value = res.total || 0
        displayedBounds = bounds
        lastUpdated.value = new Date().toISOString()
        failureCount.value = 0
        loadError.value = false
      } catch (err: unknown) {
        if (disposed || ownGeneration !== generation) continue
        if (!handleDisabled(err)) {
          loadError.value = true
          failureCount.value++
        }
      } finally {
        if (ownGeneration === generation) loading.value = false
      }
    }
  } finally {
    inFlight = false
    if (!disposed) schedulePoll()
  }
}

function pauseLive() {
  manuallyPaused.value = true
  liveEnabled.value = false
  freeze()
}
function resumeLatest() {
  manuallyPaused.value = false
  selectedLog.value = null
  page.value = 1
  filters.end_time = ''
  appliedQuery.value = { ...appliedQuery.value, end_time: undefined }
  frozenBounds = null
  liveEnabled.value = true
  void fetchLogs()
}
function openDetails(row: OpsSystemLog) {
  selectedLog.value = row
  freeze()
}
function closeDetails() {
  selectedLog.value = null
  if (canPoll()) {
    frozenBounds = null
    void fetchLogs()
  }
}
function onVisibilityChange() {
  visible.value = document.visibilityState !== 'hidden'
  if (!liveEnabled.value) return
  if (!visible.value) freeze()
  else if (canPoll()) {
    frozenBounds = null
    void fetchLogs()
  }
}

const fetchHealth = async () => {
  try {
    const result = await opsAPI.getSystemLogSinkHealth()
    if (!disposed) health.value = result
  } catch (err: unknown) {
    if (!disposed) handleDisabled(err)
    // Health is supplementary; never toast on background failures.
  }
}

const loadRuntimeConfig = async () => {
  runtimeLoading.value = true
  runtimeConfigLoaded.value = false
  try {
    const cfg = await opsAPI.getRuntimeLogConfig()
    if (disposed) return
    runtimeConfig.level = cfg.level
    runtimeConfig.persist_request_diagnostics = cfg.persist_request_diagnostics ?? false
    runtimeConfig.persist_access_logs = cfg.persist_access_logs
    runtimeConfig.enable_sampling = cfg.enable_sampling
    runtimeConfig.sampling_initial = cfg.sampling_initial
    runtimeConfig.sampling_thereafter = cfg.sampling_thereafter
    runtimeConfig.caller = cfg.caller
    runtimeConfig.stacktrace_level = cfg.stacktrace_level
    runtimeConfig.retention_days = cfg.retention_days
    runtimeConfig.request_retention_days = cfg.request_retention_days ?? 90
    loadDiagnosticPolicy(cfg)
    runtimeConfigLoaded.value = true
  } catch (err: any) {
    console.error('[OpsSystemLogTable] Failed to load runtime log config', err)
    if (!disposed && !handleDisabled(err)) appStore.showError(t('admin.ops.systemLogs.runtimeConfigLoadFailed'))
  } finally {
    runtimeLoading.value = false
  }
}

const saveRuntimeConfig = async () => {
  if (!runtimeConfigLoaded.value || runtimeLoading.value) return
  if (!Number.isInteger(runtimeConfig.retention_days) || runtimeConfig.retention_days < 1 || runtimeConfig.retention_days > 3650 ||
      !Number.isInteger(runtimeConfig.request_retention_days) || runtimeConfig.request_retention_days < 0 || runtimeConfig.request_retention_days > 3650) {
    appStore.showError(t('admin.ops.systemLogs.retentionDaysInvalid'))
    return
  }
  const days = runtimeConfig.diagnostic_retention_days
  const maxSizeMB = runtimeConfig.diagnostic_max_size_mb
  if (days == null || !Number.isInteger(days) || days < 1 || days > 3650 ||
      maxSizeMB == null || !Number.isInteger(maxSizeMB) || maxSizeMB < 0 || maxSizeMB > 102400) {
    appStore.showError(t('admin.ops.systemLogs.diagnosticPolicyInvalid'))
    return
  }
  const previous = savedDiagnosticPolicy.value
  if ((days < previous.days || (maxSizeMB > 0 && (previous.maxSizeMB === 0 || maxSizeMB < previous.maxSizeMB))) &&
      !window.confirm(t('admin.ops.systemLogs.diagnosticPolicyConfirm'))) return
  runtimeSaving.value = true
  try {
    const saved = await opsAPI.updateRuntimeLogConfig({ ...runtimeConfig })
    runtimeConfig.level = saved.level
    runtimeConfig.persist_request_diagnostics = saved.persist_request_diagnostics ?? false
    runtimeConfig.persist_access_logs = saved.persist_access_logs
    runtimeConfig.enable_sampling = saved.enable_sampling
    runtimeConfig.sampling_initial = saved.sampling_initial
    runtimeConfig.sampling_thereafter = saved.sampling_thereafter
    runtimeConfig.caller = saved.caller
    runtimeConfig.stacktrace_level = saved.stacktrace_level
    runtimeConfig.retention_days = saved.retention_days
    runtimeConfig.request_retention_days = saved.request_retention_days ?? 90
    loadDiagnosticPolicy(saved)
    appStore.showSuccess(t('admin.ops.systemLogs.runtimeConfigActive'))
  } catch (err: any) {
    console.error('[OpsSystemLogTable] Failed to save runtime log config', err)
    if (!handleDisabled(err)) appStore.showError(err?.response?.data?.detail || t('admin.ops.systemLogs.runtimeConfigSaveFailed'))
  } finally {
    runtimeSaving.value = false
  }
}

const resetRuntimeConfig = async () => {
  const ok = window.confirm(t('admin.ops.systemLogs.resetRuntimeConfigConfirm'))
  if (!ok) return

  runtimeSaving.value = true
  try {
    const saved = await opsAPI.resetRuntimeLogConfig()
    runtimeConfig.level = saved.level
    runtimeConfig.persist_request_diagnostics = saved.persist_request_diagnostics ?? false
    runtimeConfig.persist_access_logs = saved.persist_access_logs
    runtimeConfig.enable_sampling = saved.enable_sampling
    runtimeConfig.sampling_initial = saved.sampling_initial
    runtimeConfig.sampling_thereafter = saved.sampling_thereafter
    runtimeConfig.caller = saved.caller
    runtimeConfig.stacktrace_level = saved.stacktrace_level
    runtimeConfig.retention_days = saved.retention_days
    runtimeConfig.request_retention_days = saved.request_retention_days ?? 90
    loadDiagnosticPolicy(saved)
    appStore.showSuccess(t('admin.ops.systemLogs.runtimeConfigReset'))
    await fetchHealth()
  } catch (err: any) {
    console.error('[OpsSystemLogTable] Failed to reset runtime log config', err)
    if (!handleDisabled(err)) appStore.showError(err?.response?.data?.detail || t('admin.ops.systemLogs.runtimeConfigResetFailed'))
  } finally {
    runtimeSaving.value = false
  }
}

const cleanupCurrentFilter = async () => {
  if (opsDisabled.value) return
  const ok = window.confirm(t('admin.ops.systemLogs.cleanupConfirm'))
  if (!ok) return
  try {
    // Clean the applied preset and displayed window, not broader unsaved filter drafts.
    const payload = { ...appliedQuery.value, ...(displayedBounds || boundsForQuery()) }
    delete payload.page
    delete payload.page_size
    delete payload.time_range
    const res = await opsAPI.cleanupSystemLogs(payload)
    appStore.showSuccess(t('admin.ops.systemLogs.cleanupSuccess', { count: res.deleted || 0 }))
    page.value = 1
    await Promise.all([fetchLogs(), fetchHealth()])
  } catch (err: any) {
    console.error('[OpsSystemLogTable] Failed to cleanup logs', err)
    appStore.showError(
      extractApiErrorMessage(err, t('admin.ops.systemLogs.cleanupFailed'), {
        OPS_SYSTEM_LOG_CLEANUP_FILTER_REQUIRED: t('admin.ops.systemLogs.cleanupFilterRequired')
      })
    )
  }
}

const resetFilters = () => {
  filters.time_range = '1h'
  filters.start_time = ''
  filters.end_time = ''
  filters.host = ''
  filters.level = ''
  filters.component = ''
  filters.request_id = ''
  filters.client_request_id = ''
  filters.user_id = ''
  filters.api_key_id = ''
  filters.account_id = ''
  filters.platform = props.platformFilter || ''
  filters.model = ''
  filters.q = ''
  if (preset.value !== 'all') preset.value = 'all'
  else applyFilters()
}

watch(() => props.platformFilter, (v) => {
  if (v && !filters.platform) {
    filters.platform = v
    applyFilters()
  }
})

watch(() => props.refreshToken, () => {
  // Ops dashboard owns refresh unless this table's Live worker is active.
  if (props.diagnosticsMode || liveEnabled.value || manuallyPaused.value || page.value !== 1 || selectedLog.value) return
  void fetchLogs()
  void fetchHealth()
})

const onPageChange = (next: number) => {
  if (next !== 1) freeze()
  else if (liveEnabled.value) frozenBounds = null
  page.value = next
  void fetchLogs()
}

const onPageSizeChange = (next: number) => {
  pageSize.value = next
  page.value = 1
  if (liveEnabled.value) frozenBounds = null
  void fetchLogs()
}

const applyFilters = () => {
  invalidate()
  page.value = 1
  selectedLog.value = null
  frozenBounds = null
  displayedBounds = null
  appliedQuery.value = buildQuery()
  if (filters.end_time) liveEnabled.value = false
  void fetchLogs()
}
watch(preset, applyFilters)
const hasData = computed(() => logs.value.length > 0)

onMounted(async () => {
  if (props.platformFilter) filters.platform = props.platformFilter
  appliedQuery.value = buildQuery()
  document.addEventListener('visibilitychange', onVisibilityChange)
  await Promise.all([fetchLogs(), fetchHealth(), loadRuntimeConfig()])
})
onUnmounted(() => {
  disposed = true
  invalidate()
  document.removeEventListener('visibilitychange', onVisibilityChange)
})
</script>

<template>
  <section class="rounded-2xl border border-gray-200 bg-white p-4 shadow-sm dark:border-dark-700 dark:bg-dark-900/60">
    <div v-if="opsDisabled" role="alert" class="rounded-lg bg-amber-50 p-4 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-200">
      {{ t('admin.ops.systemLogs.monitoringDisabled') }}
    </div>
    <template v-else>
    <div class="mb-4 flex flex-wrap items-center justify-between gap-3">
      <div>
        <h3 class="text-sm font-bold text-gray-900 dark:text-white">{{ t(diagnosticsMode ? 'admin.ops.systemLogs.debugTitle' : 'admin.ops.systemLogs.title') }}</h3>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.description') }}</p>
        <p class="mt-2 max-w-3xl text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.diagnosticsHint') }}</p>
      </div>
      <div class="flex flex-wrap items-center gap-2 text-xs">
        <span class="rounded-md bg-gray-100 px-2 py-1 text-gray-700 dark:bg-dark-700 dark:text-gray-200">{{ t('admin.ops.systemLogs.queue') }} {{ health.queue_depth }}/{{ health.queue_capacity }}</span>
        <span class="rounded-md bg-gray-100 px-2 py-1 text-gray-700 dark:bg-dark-700 dark:text-gray-200">{{ t('admin.ops.systemLogs.written') }} {{ health.written_count }}</span>
        <span class="rounded-md bg-amber-100 px-2 py-1 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300">{{ t('admin.ops.systemLogs.dropped') }} {{ health.dropped_count }}</span>
        <span class="rounded-md bg-red-100 px-2 py-1 text-red-700 dark:bg-red-900/30 dark:text-red-300">{{ t('admin.ops.systemLogs.failed') }} {{ health.write_failed_count }}</span>
      </div>
    </div>

    <div class="mb-4 rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-700 dark:bg-dark-800/70">
      <div class="mb-2 flex items-center justify-between">
        <div class="text-xs font-semibold text-gray-700 dark:text-gray-200">{{ t('admin.ops.systemLogs.runtimeConfig') }}</div>
        <span v-if="runtimeLoading" class="text-xs text-gray-500">{{ t('common.loading') }}</span>
      </div>
      <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-6">
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.level') }}
          <Select v-model="runtimeConfig.level" class="mt-1" :options="runtimeLevelOptions" />
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.stacktraceThreshold') }}
          <Select v-model="runtimeConfig.stacktrace_level" class="mt-1" :options="stacktraceLevelOptions" />
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.samplingInitial') }}
          <input v-model.number="runtimeConfig.sampling_initial" type="number" min="1" class="input mt-1" />
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.samplingThereafter') }}
          <input v-model.number="runtimeConfig.sampling_thereafter" type="number" min="1" class="input mt-1" />
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.retentionDays') }}
          <LogRetentionSelect v-model="runtimeConfig.retention_days" :label="t('admin.ops.systemLogs.retentionDays')" class="mt-1" />
          <span class="mt-1 block text-[11px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.retentionDaysHint') }}</span>
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.requestRetentionDays') }}
          <LogRetentionSelect v-model="runtimeConfig.request_retention_days" :label="t('admin.ops.systemLogs.requestRetentionDays')" allow-forever class="mt-1" />
          <span class="mt-1 block text-[11px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.requestRetentionDaysHint') }}</span>
        </label>
        <fieldset class="rounded-lg border border-gray-200 p-3 dark:border-dark-700 md:col-span-2 xl:col-span-6" data-testid="diagnostic-retention-policy">
          <legend class="px-1 text-xs font-medium text-gray-700 dark:text-gray-200">{{ t('admin.ops.systemLogs.diagnosticPolicyTitle') }}</legend>
          <div class="grid gap-4 sm:grid-cols-2">
            <label class="text-xs text-gray-600 dark:text-gray-300">
              {{ t('admin.ops.systemLogs.diagnosticRetentionDays') }}
              <LogRetentionSelect v-model="runtimeConfig.diagnostic_retention_days" :label="t('admin.ops.systemLogs.diagnosticRetentionDays')" class="mt-1" data-testid="diagnostic-retention-days" />
            </label>
            <label class="text-xs text-gray-600 dark:text-gray-300">
              {{ t('admin.ops.systemLogs.diagnosticMaxSizeMB') }}
              <input v-model.number="runtimeConfig.diagnostic_max_size_mb" type="number" min="0" max="102400" step="1" class="input mt-1" data-testid="diagnostic-max-size-mb" />
              <span class="mt-1 block text-[11px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.diagnosticSizeHint') }}</span>
            </label>
          </div>
          <p class="mt-2 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.diagnosticPolicyHint') }}</p>
          <p v-if="health.diagnostic_retention?.result?.scan_incomplete" role="alert" class="mt-2 text-xs text-amber-700 dark:text-amber-300" data-testid="diagnostic-retention-incomplete">{{ t('admin.ops.systemLogs.diagnosticPolicyIncomplete') }}</p>
          <p v-if="health.diagnostic_retention?.last_error" role="alert" class="mt-2 text-xs text-red-600 dark:text-red-400">{{ t('admin.ops.systemLogs.diagnosticPolicyError') }}</p>
        </fieldset>
        <div class="md:col-span-2 xl:col-span-6">
          <div class="grid gap-3 lg:grid-cols-[minmax(0,1fr)_auto] lg:items-end">
            <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
              <label class="inline-flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
                <input v-model="runtimeConfig.caller" type="checkbox" />
                {{ t('admin.ops.systemLogs.caller') }}
              </label>
              <label class="inline-flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
                <input v-model="runtimeConfig.enable_sampling" type="checkbox" />
                {{ t('admin.ops.systemLogs.sampling') }}
              </label>
              <label class="inline-flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
                <input v-model="runtimeConfig.persist_access_logs" type="checkbox" />
                {{ t('admin.ops.systemLogs.persistAccessLogs') }}
              </label>
              <label class="inline-flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
                <input v-model="runtimeConfig.persist_request_diagnostics" type="checkbox" data-testid="persist-request-diagnostics" />
                {{ t('admin.ops.systemLogs.persistRequestDiagnostics') }}
              </label>
            </div>
            <div class="flex flex-wrap items-center gap-2 lg:justify-end">
              <button type="button" class="btn btn-primary btn-sm" :disabled="runtimeSaving || runtimeLoading || !runtimeConfigLoaded" @click="saveRuntimeConfig">
                {{ runtimeSaving ? t('common.saving') : t('admin.ops.systemLogs.saveAndApply') }}
              </button>
              <button type="button" class="btn btn-secondary btn-sm" :disabled="runtimeSaving" @click="resetRuntimeConfig">
                {{ t('admin.ops.systemLogs.resetDefaults') }}
              </button>
            </div>
          </div>
        </div>
      </div>
      <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.persistAccessLogsHint') }}</p>
      <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.persistRequestDiagnosticsHint') }}</p>
      <p v-if="health.last_error" class="mt-2 text-xs text-red-600 dark:text-red-400">{{ t('admin.ops.systemLogs.latestWriteError') }} {{ health.last_error }}</p>
    </div>

    <div class="mb-4 flex flex-wrap items-end gap-3">
      <label class="min-w-48 text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.preset') }}
        <Select v-model="preset" class="mt-1" :options="presetOptions" data-testid="log-preset" />
      </label>
      <button v-if="liveEnabled" type="button" class="btn btn-secondary btn-sm" data-testid="pause-live" @click="pauseLive">{{ t('admin.ops.systemLogs.pauseLive') }}</button>
      <button type="button" class="btn btn-secondary btn-sm" data-testid="resume-latest" @click="resumeLatest">{{ t('admin.ops.systemLogs.resumeLatest') }}</button>
      <span class="text-xs text-gray-500 dark:text-gray-400" role="status">{{ t(`admin.ops.systemLogs.${liveStatus}`) }}</span>
      <span v-if="lastUpdated" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.lastUpdated') }} {{ formatTime(lastUpdated) }}</span>
    </div>
    <p class="mb-3 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.protocolHint') }}</p>
    <div v-if="loadError" role="alert" class="mb-3 text-sm text-amber-700 dark:text-amber-300">{{ t('admin.ops.systemLogs.loadFailed') }} {{ t('admin.ops.systemLogs.retryHint') }}</div>

    <div class="mb-4 grid grid-cols-1 gap-3 md:grid-cols-5">
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.timeRange') }}
        <Select v-model="filters.time_range" class="mt-1" :options="timeRangeOptions" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.startTime') }}
        <input v-model="filters.start_time" type="datetime-local" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.endTime') }}
        <input v-model="filters.end_time" type="datetime-local" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.level') }}
        <Select v-model="filters.level" class="mt-1" :options="filterLevelOptions" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.component') }}
        <input v-model="filters.component" type="text" :disabled="preset === 'access'" class="input mt-1" :placeholder="t('admin.ops.systemLogs.componentPlaceholder')" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.host') }}
        <input v-model="filters.host" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        request_id
        <input v-model="filters.request_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        client_request_id
        <input v-model="filters.client_request_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        user_id
        <input v-model="filters.user_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.keyId') }}
        <input v-model="filters.api_key_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        account_id
        <input v-model="filters.account_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.platform') }}
        <input v-model="filters.platform" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.model') }}
        <input v-model="filters.model" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.keyword') }}
        <input v-model="filters.q" type="text" class="input mt-1" :placeholder="t('admin.ops.systemLogs.keywordPlaceholder')" />
      </label>
    </div>

    <div class="mb-3 flex flex-wrap gap-2">
      <button type="button" class="btn btn-primary btn-sm" @click="applyFilters">{{ t('admin.ops.systemLogs.search') }}</button>
      <button type="button" class="btn btn-secondary btn-sm" @click="resetFilters">{{ t('common.reset') }}</button>
      <button type="button" class="btn btn-danger btn-sm" @click="cleanupCurrentFilter">{{ t('admin.ops.systemLogs.cleanCurrentFilters') }}</button>
      <button type="button" class="btn btn-secondary btn-sm" @click="fetchHealth">{{ t('admin.ops.systemLogs.refreshHealth') }}</button>
    </div>

    <div class="overflow-hidden rounded-xl border border-gray-200 dark:border-dark-700">
      <div v-if="loading" class="px-4 py-8 text-center text-sm text-gray-500">{{ t('common.loading') }}</div>
      <div v-else-if="!hasData" class="px-4 py-8 text-center text-sm text-gray-500">{{ t('admin.ops.systemLogs.empty') }}</div>
      <div v-else-if="!isDesktopViewport" class="divide-y divide-gray-100 dark:divide-dark-800">
        <div v-for="row in logs" :key="row.id" class="space-y-1.5 p-3">
          <div class="flex items-center justify-between gap-2">
            <span class="inline-flex rounded-full px-2 py-0.5 text-xs font-semibold" :class="levelBadgeClass(row.level)">
              {{ row.level }}
            </span>
            <span class="text-xs text-gray-500 dark:text-gray-400">{{ formatTime(row.created_at) }}</span>
          </div>
          <div v-if="row.host" class="truncate text-xs text-gray-500 dark:text-gray-400" :title="row.host">
            {{ row.host }}
          </div>
          <div class="whitespace-normal break-all text-xs text-gray-700 dark:text-gray-300">
            <template v-if="isDiagnostic(row)">
              <RequestDiagnosticDetails :row="row" summary />
              <button type="button" class="btn btn-secondary btn-sm mt-2" data-testid="diagnostic-details" @click="openDetails(row)">{{ t('admin.ops.systemLogs.phaseDetails') }}</button>
            </template>
            <template v-else>{{ formatSystemLogDetail(row) }}</template>
          </div>
        </div>
      </div>
      <div v-else class="overflow-auto">
        <table class="min-w-full table-fixed divide-y divide-gray-200 dark:divide-dark-700">
          <thead class="bg-gray-50 dark:bg-dark-900">
            <tr>
              <th class="w-[170px] px-3 py-2 text-left text-[11px] font-semibold text-gray-500">{{ t('admin.ops.systemLogs.time') }}</th>
              <th class="w-[160px] px-3 py-2 text-left text-[11px] font-semibold text-gray-500">{{ t('admin.ops.systemLogs.host') }}</th>
              <th class="w-[80px] px-3 py-2 text-left text-[11px] font-semibold text-gray-500">{{ t('admin.ops.systemLogs.level') }}</th>
              <th class="px-3 py-2 text-left text-[11px] font-semibold text-gray-500">{{ t('admin.ops.systemLogs.logDetails') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 dark:divide-dark-800">
            <tr v-for="row in logs" :key="row.id" class="align-top">
              <td class="px-3 py-2 text-xs text-gray-700 dark:text-gray-300">{{ formatTime(row.created_at) }}</td>
              <td class="px-3 py-2 text-xs text-gray-700 dark:text-gray-300">
                <span class="block truncate" :title="row.host || '-'">{{ row.host || '-' }}</span>
              </td>
              <td class="px-3 py-2 text-xs">
                <span class="inline-flex rounded-full px-2 py-0.5 font-semibold" :class="levelBadgeClass(row.level)">
                  {{ row.level }}
                </span>
              </td>
              <td class="px-3 py-2 text-xs text-gray-700 dark:text-gray-300 whitespace-normal break-all">
                <template v-if="isDiagnostic(row)">
                  <RequestDiagnosticDetails :row="row" summary />
                  <button type="button" class="btn btn-secondary btn-sm mt-2" data-testid="diagnostic-details" @click="openDetails(row)">{{ t('admin.ops.systemLogs.phaseDetails') }}</button>
                </template>
                <template v-else>{{ formatSystemLogDetail(row) }}</template>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <Pagination
        :total="total"
        :page="page"
        :page-size="pageSize"
        @update:page="onPageChange"
        @update:page-size="onPageSizeChange"
      />
    </div>
    <BaseDialog :show="selectedLog !== null" :title="t('admin.ops.systemLogs.phaseDetails')" width="extra-wide" @close="closeDetails">
      <div v-if="selectedLog" class="space-y-4">
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ formatTime(selectedLog.created_at) }} / {{ selectedLog.host || t('admin.ops.systemLogs.notRecorded') }}</p>
        <RequestDiagnosticDetails :row="selectedLog" />
      </div>
    </BaseDialog>
    </template>
  </section>
</template>
