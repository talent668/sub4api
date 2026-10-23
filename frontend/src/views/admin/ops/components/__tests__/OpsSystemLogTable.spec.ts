import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import OpsSystemLogTable from '../OpsSystemLogTable.vue'
import LogRetentionSelect from '../LogRetentionSelect.vue'
import RequestDiagnosticDetails from '../RequestDiagnosticDetails.vue'
import enLocale from '@/i18n/locales/en'
import zhLocale from '@/i18n/locales/zh'

const mockListSystemLogs = vi.fn()
const mockCleanupSystemLogs = vi.fn()
const mockGetSystemLogSinkHealth = vi.fn()
const mockGetRuntimeLogConfig = vi.fn()
const mockUpdateRuntimeLogConfig = vi.fn()
const mockResetRuntimeLogConfig = vi.fn()
const mockShowError = vi.fn()

vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }))

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    listSystemLogs: (...args: any[]) => mockListSystemLogs(...args),
    cleanupSystemLogs: (...args: any[]) => mockCleanupSystemLogs(...args),
    getSystemLogSinkHealth: (...args: any[]) => mockGetSystemLogSinkHealth(...args),
    getRuntimeLogConfig: (...args: any[]) => mockGetRuntimeLogConfig(...args),
    updateRuntimeLogConfig: (...args: any[]) => mockUpdateRuntimeLogConfig(...args),
    resetRuntimeLogConfig: (...args: any[]) => mockResetRuntimeLogConfig(...args),
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError: (...args: any[]) => mockShowError(...args),
    showSuccess: vi.fn(),
  }),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const SelectStub = defineComponent({
  name: 'SelectControlStub',
  props: {
    modelValue: {
      type: [String, Number],
      default: '',
    },
  },
  emits: ['update:modelValue'],
  template: '<div class="select-stub" />',
})

const PaginationStub = defineComponent({
  name: 'PaginationStub',
  template: '<div class="pagination-stub" />',
})

const runtimeConfig = {
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
}

const sinkHealth = {
  queue_depth: 0,
  queue_capacity: 5000,
  dropped_count: 0,
  write_failed_count: 0,
  written_count: 1,
  avg_write_delay_ms: 0,
}

const diagnosticRow = {
  id: 91, created_at: '2026-10-23T09:00:00Z', host: 'gateway-1', level: 'debug', component: 'gateway',
  message: 'openai.messages_ws_latency', request_id: 'safe-request-id', api_key_id: 42,
  extra: {
    event: 'openai.messages_ws_latency', prep_ms: 0, conn_reused: false, force_new_conn: true,
    first_semantic_ms: 0, first_text_ms: 'not-a-number', attempt_duration_ms: 21,
    transport: 'websocket', outcome: 'completed', request_body: 'SECRET_BODY', authorization: 'SECRET_HEADER',
  },
}
const emptyLogs = { items: [], total: 0, page: 1, page_size: 20 }
const diagnosticLogs = { ...emptyLogs, items: [diagnosticRow], total: 1 }
function mountDiagnostics(diagnosticsMode = true) {
  const wrapper = mount(OpsSystemLogTable, {
    props: { diagnosticsMode },
    global: {
      stubs: {
        Select: SelectStub, Pagination: PaginationStub,
        BaseDialog: defineComponent({
          props: ['show'], emits: ['close'],
          template: '<div v-if="show" data-testid="detail-dialog"><slot /><button data-testid="close-detail" @click="$emit(\'close\')">Close</button></div>',
        }),
      },
    },
  })
  activeWrappers.push(wrapper)
  return wrapper
}

const activeWrappers: Array<{ unmount: () => void }> = []
afterEach(() => {
  activeWrappers.splice(0).forEach(wrapper => wrapper.unmount())
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('OpsSystemLogTable host support', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockListSystemLogs.mockResolvedValue({
      items: [
        {
          id: 1,
          created_at: '2026-07-14T00:10:01Z',
          host: 'api-node-1',
          level: 'warn',
          component: 'app',
          message: 'request failed',
        },
      ],
      total: 1,
      page: 1,
      page_size: 20,
    })
    mockCleanupSystemLogs.mockResolvedValue({ deleted: 1 })
    mockGetSystemLogSinkHealth.mockResolvedValue(sinkHealth)
    mockGetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
    mockUpdateRuntimeLogConfig.mockImplementation(async config => config)
  })

  it('renders the host and sends it with list and cleanup filters', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('api-node-1')

    const hostLabel = wrapper.findAll('label').find((label) => label.text().includes('admin.ops.systemLogs.host'))
    expect(hostLabel).toBeDefined()
    await hostLabel!.find('input').setValue(' api-node-2 ')

    const searchButton = wrapper.findAll('button').find((button) => button.text() === 'admin.ops.systemLogs.search')
    expect(searchButton).toBeDefined()
    await searchButton!.trigger('click')
    await flushPromises()

    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ host: 'api-node-2' }))

    const cleanupButton = wrapper.findAll('button').find((button) => button.text() === 'admin.ops.systemLogs.cleanCurrentFilters')
    expect(cleanupButton).toBeDefined()
    await cleanupButton!.trigger('click')
    await flushPromises()

    expect(mockCleanupSystemLogs).toHaveBeenCalledWith(expect.objectContaining({ host: 'api-node-2' }))
    wrapper.unmount()
  })

  it('keeps database access-log persistence opt-in', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    const label = wrapper.findAll('label').find((item) => item.text().includes('admin.ops.systemLogs.persistAccessLogs'))
    expect(label).toBeDefined()
    expect((label!.find('input').element as HTMLInputElement).checked).toBe(false)
    wrapper.unmount()
  })

  it.each([
    ['zh', zhLocale],
    ['en', enLocale],
  ])('defines the Host translation for %s', (_name, locale) => {
    expect(locale.admin.ops.systemLogs.host).toBe('Host')
  })
})


describe('request diagnostics and Live lifecycle', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-23T10:00:00Z'))
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockListSystemLogs.mockResolvedValue(diagnosticLogs)
    mockCleanupSystemLogs.mockResolvedValue({ deleted: 1 })
    mockGetSystemLogSinkHealth.mockResolvedValue(sinkHealth)
    mockGetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
    mockUpdateRuntimeLogConfig.mockImplementation(async config => config)
    mockResetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
  })

  it('uses exact event and existing access component presets, including cleanup', async () => {
    const wrapper = mountDiagnostics()
    await flushPromises()
    expect(wrapper.text()).toContain('safe-request-id')
    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ event: 'openai.messages_ws_latency' }))
    expect(mockListSystemLogs.mock.lastCall![0].q).toBeUndefined()
    await wrapper.findAll('button').find(button => button.text() === 'admin.ops.systemLogs.cleanCurrentFilters')!.trigger('click')
    await flushPromises()
    expect(mockCleanupSystemLogs).toHaveBeenCalledWith(expect.objectContaining({ event: 'openai.messages_ws_latency' }))
    const selector = wrapper.findAllComponents(SelectStub).find(select => select.props('modelValue') === 'diagnostics')!
    selector.vm.$emit('update:modelValue', 'access')
    await flushPromises()
    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ component: 'http.access' }))
    expect(mockListSystemLogs.mock.lastCall![0].event).toBeUndefined()
    selector.vm.$emit('update:modelValue', 'all')
    await flushPromises()
    expect(mockListSystemLogs.mock.lastCall![0].component).toBeUndefined()
    expect(mockListSystemLogs.mock.lastCall![0].event).toBeUndefined()
  })

  it('preserves zero/false, rejects wrong types and never shows arbitrary extras', () => {
    const wrapper = mount(RequestDiagnosticDetails, { props: { row: diagnosticRow } })
    activeWrappers.push(wrapper)
    expect(wrapper.text()).toContain('0 ms')
    expect(wrapper.text()).toContain('false')
    expect(wrapper.text()).toContain('true')
    expect(wrapper.text()).toContain('admin.ops.systemLogs.notRecorded')
    expect(wrapper.text()).toContain('safe-request-id')
    expect(wrapper.text()).not.toContain('not-a-number')
    expect(wrapper.text()).not.toContain('SECRET_BODY')
    expect(wrapper.text()).not.toContain('SECRET_HEADER')
  })

  it('loads, saves and resets diagnostics independently of level and access persistence', async () => {
    const wrapper = mountDiagnostics()
    await flushPromises()
    expect((wrapper.get('[data-testid="persist-request-diagnostics"]').element as HTMLInputElement).checked).toBe(false)
    await wrapper.get('[data-testid="persist-request-diagnostics"]').setValue(true)
    await wrapper.findAll('button').find(button => button.text() === 'admin.ops.systemLogs.saveAndApply')!.trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ persist_request_diagnostics: true, level: 'info', persist_access_logs: false }))
    await wrapper.findAll('button').find(button => button.text() === 'admin.ops.systemLogs.resetDefaults')!.trigger('click')
    await flushPromises()
    expect(mockResetRuntimeLogConfig).toHaveBeenCalledTimes(1)
    expect((wrapper.get('[data-testid="persist-request-diagnostics"]').element as HTMLInputElement).checked).toBe(false)
  })

  it('polls at 2s without dashboard duplication and stops on unmount', async () => {
    const wrapper = mountDiagnostics()
    await flushPromises()
    await wrapper.setProps({ refreshToken: 1 })
    expect(mockListSystemLogs).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(2000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(2)
    expect(mockGetSystemLogSinkHealth).toHaveBeenCalledTimes(1)
    wrapper.unmount()
    activeWrappers.pop()
    await vi.advanceTimersByTimeAsync(20000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(2)
  })

  it('freezes page bounds and resumes at latest page one', async () => {
    const wrapper = mountDiagnostics()
    await flushPromises()
    const initial = mockListSystemLogs.mock.lastCall![0]
    await vi.advanceTimersByTimeAsync(1000)
    wrapper.findComponent(PaginationStub).vm.$emit('update:page', 2)
    await flushPromises()
    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ page: 2, start_time: initial.start_time, end_time: initial.end_time }))
    await vi.advanceTimersByTimeAsync(6000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(2)
    await wrapper.get('[data-testid="resume-latest"]').trigger('click')
    await flushPromises()
    expect(mockListSystemLogs.mock.lastCall![0].page).toBe(1)
    expect(mockListSystemLogs.mock.lastCall![0].end_time).not.toBe(initial.end_time)
  })

  it('pauses explicitly and while details or the document are hidden', async () => {
    const wrapper = mountDiagnostics()
    await flushPromises()
    await wrapper.get('[data-testid="diagnostic-details"]').trigger('click')
    await vi.advanceTimersByTimeAsync(6000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(1)
    await wrapper.get('[data-testid="close-detail"]').trigger('click')
    await flushPromises()
    expect(mockListSystemLogs).toHaveBeenCalledTimes(2)
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(6000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(2)
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
    document.dispatchEvent(new Event('visibilitychange'))
    await flushPromises()
    expect(mockListSystemLogs).toHaveBeenCalledTimes(3)
    await wrapper.get('[data-testid="pause-live"]').trigger('click')
    await vi.advanceTimersByTimeAsync(6000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(3)
  })

  it('serializes filter changes and ignores stale responses', async () => {
    let finish: (value: typeof diagnosticLogs) => void = () => {}
    mockListSystemLogs.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = mountDiagnostics()
    await flushPromises()
    const host = wrapper.findAll('label').find(label => label.text().includes('admin.ops.systemLogs.host'))!
    await host.get('input').setValue('new-host')
    await wrapper.findAll('button').find(button => button.text() === 'admin.ops.systemLogs.search')!.trigger('click')
    expect(mockListSystemLogs).toHaveBeenCalledTimes(1)
    mockListSystemLogs.mockResolvedValue(emptyLogs)
    finish(diagnosticLogs)
    await flushPromises()
    expect(mockListSystemLogs).toHaveBeenCalledTimes(2)
    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ host: 'new-host' }))
    expect(wrapper.text()).not.toContain('gateway-1')
  })

  it('backs off transient errors without toast spam and respects monitoring disabled', async () => {
    mockListSystemLogs.mockRejectedValue(new Error('offline'))
    const wrapper = mountDiagnostics()
    await flushPromises()
    expect(mockShowError).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(2000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(2000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(2)
    mockListSystemLogs.mockRejectedValue({ code: 'OPS_DISABLED' })
    await vi.advanceTimersByTimeAsync(8000)
    expect(wrapper.text()).toContain('admin.ops.systemLogs.monitoringDisabled')
    const calls = mockListSystemLogs.mock.calls.length
    await vi.advanceTimersByTimeAsync(60000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(calls)
    expect(mockShowError).not.toHaveBeenCalled()
  })

  it('preserves dashboard refresh for existing Ops use without autonomous polling', async () => {
    const wrapper = mountDiagnostics(false)
    await flushPromises()
    expect(mockListSystemLogs.mock.lastCall![0].event).toBeUndefined()
    await vi.advanceTimersByTimeAsync(6000)
    expect(mockListSystemLogs).toHaveBeenCalledTimes(1)
    await wrapper.setProps({ refreshToken: 2 })
    await flushPromises()
    expect(mockListSystemLogs).toHaveBeenCalledTimes(2)
  })

  it.each([enLocale, zhLocale])('defines diagnostics copy and every allowlisted field', locale => {
    const strings = locale.admin.ops.systemLogs
    expect(strings.notRecorded).toBeTruthy()
    expect(strings.persistRequestDiagnostics).toBeTruthy()
    for (const field of ['prep_ms', 'acquire_ms', 'prewrite_ms', 'write_ms', 'upstream_first_event_wait_ms', 'upstream_first_semantic_wait_ms', 'first_semantic_ms', 'first_text_ms', 'attempt_duration_ms', 'conn_reused', 'force_new_conn', 'conn_pick_ms', 'queue_wait_ms', 'ingress', 'transport', 'outcome']) {
      expect(strings.phases[field as keyof typeof strings.phases]).toBeTruthy()
    }
  })
})

describe('rolling log retention settings', () => {
  const mountTable = () => mount(OpsSystemLogTable, {
    global: { stubs: { Select: SelectStub, Pagination: PaginationStub } }
  })
  const saveButton = (wrapper: ReturnType<typeof mountTable>) => wrapper.findAll('button').find(button => button.text() === 'admin.ops.systemLogs.saveAndApply')!

  beforeEach(() => {
    vi.clearAllMocks()
    mockListSystemLogs.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20 })
    mockGetSystemLogSinkHealth.mockResolvedValue(sinkHealth)
    mockGetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
    mockUpdateRuntimeLogConfig.mockImplementation(async config => config)
  })

  it('loads and saves separate operations and request retention windows', async () => {
    const wrapper = mountTable()
    await flushPromises()
    const fields = wrapper.findAllComponents(LogRetentionSelect)
    expect(fields.map(field => field.props('modelValue'))).toEqual([30, 90, 7])
    fields[0]!.vm.$emit('update:modelValue', 7)
    fields[1]!.vm.$emit('update:modelValue', 180)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ retention_days: 7, request_retention_days: 180 }))
    wrapper.unmount()
  })

  it('supports forever and rejects fractional or empty custom days', async () => {
    const wrapper = mountTable()
    await flushPromises()
    const requestField = wrapper.findAllComponents(LogRetentionSelect)[1]!
    requestField.vm.$emit('update:modelValue', 0)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ request_retention_days: 0 }))
    mockUpdateRuntimeLogConfig.mockClear()
    for (const value of [1.5, Number.NaN, -1, 3651]) {
      requestField.vm.$emit('update:modelValue', value)
      await saveButton(wrapper).trigger('click')
    }
    expect(mockUpdateRuntimeLogConfig).not.toHaveBeenCalled()
    expect(mockShowError).toHaveBeenCalledWith('admin.ops.systemLogs.retentionDaysInvalid')
    wrapper.unmount()
  })

  it('defaults diagnostics to seven days with no capacity limit and preserves other policies', async () => {
    const wrapper = mountTable()
    await flushPromises()
    expect(wrapper.get('[data-testid="diagnostic-max-size-mb"]').element).toHaveProperty('value', '0')
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ diagnostic_retention_days: 7, diagnostic_max_size_mb: 0, retention_days: 30, request_retention_days: 90 }))
    wrapper.unmount()
  })

  it('confirms only tighter diagnostic limits and permits canceling without saving', async () => {
    mockGetRuntimeLogConfig.mockResolvedValue({ ...runtimeConfig, diagnostic_retention_days: 14, diagnostic_max_size_mb: 100 })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const wrapper = mountTable()
    await flushPromises()
    wrapper.findAllComponents(LogRetentionSelect)[2]!.vm.$emit('update:modelValue', 7)
    await saveButton(wrapper).trigger('click')
    expect(confirm).toHaveBeenCalledWith('admin.ops.systemLogs.diagnosticPolicyConfirm')
    expect(mockUpdateRuntimeLogConfig).not.toHaveBeenCalled()
    confirm.mockReturnValue(true)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ diagnostic_retention_days: 7, diagnostic_max_size_mb: 100 }))
    mockUpdateRuntimeLogConfig.mockClear()
    confirm.mockClear()
    await wrapper.get('[data-testid="diagnostic-max-size-mb"]').setValue('0')
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(confirm).not.toHaveBeenCalled()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ diagnostic_max_size_mb: 0 }))
    confirm.mockRestore()
    wrapper.unmount()
  })

  it('rejects invalid diagnostic days and capacities before saving', async () => {
    const wrapper = mountTable()
    await flushPromises()
    const field = wrapper.findAllComponents(LogRetentionSelect)[2]!
    for (const value of [0, -1, 1.5, 3651, Number.NaN]) {
      field.vm.$emit('update:modelValue', value)
      await saveButton(wrapper).trigger('click')
    }
    field.vm.$emit('update:modelValue', 7)
    for (const value of ['-1', '0.5', '102401', '']) {
      await wrapper.get('[data-testid="diagnostic-max-size-mb"]').setValue(value)
      await saveButton(wrapper).trigger('click')
    }
    expect(mockUpdateRuntimeLogConfig).not.toHaveBeenCalled()
    expect(mockShowError).toHaveBeenCalledWith('admin.ops.systemLogs.diagnosticPolicyInvalid')
    wrapper.unmount()
  })

  it('does not overwrite saved retention with defaults after a load failure', async () => {
    mockGetRuntimeLogConfig.mockRejectedValue(new Error('unavailable'))
    const wrapper = mountTable()
    await flushPromises()
    expect(saveButton(wrapper).attributes('disabled')).toBeDefined()
    await saveButton(wrapper).trigger('click')
    expect(mockUpdateRuntimeLogConfig).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
