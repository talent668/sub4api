import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AuditLogView from '../AuditLogView.vue'

const { list, showError } = vi.hoisted(() => ({ list: vi.fn(), showError: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { audit: { list, get: vi.fn(), clear: vi.fn() } } }))
vi.mock('@/api', () => ({ totpAPI: { getStatus: vi.fn() } }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError, showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))
vi.mock('../ops/components/OpsSystemLogTable.vue', () => ({
  default: { name: 'OpsSystemLogTable', props: { diagnosticsMode: Boolean }, template: '<section data-testid="debug-table" />' }
}))

async function mountLogs(path = '/admin/audit-logs') {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/admin/audit-logs', component: AuditLogView }] })
  await router.push(path)
  await router.isReady()
  const wrapper = mount(AuditLogView, {
    global: {
      plugins: [router],
      stubs: {
        AppLayout: { template: '<main><slot /></main>' },
        TablePageLayout: { template: '<section data-testid="audit-table"><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>' },
        OpsSystemLogTable: { name: 'OpsSystemLogTable', props: { diagnosticsMode: Boolean }, template: '<section data-testid="debug-table" />' },
        DataTable: true, Pagination: true, Select: true, BaseDialog: true, ConfirmDialog: true, Icon: true
      }
    }
  })
  await flushPromises()
  return { wrapper, router }
}

beforeEach(() => { vi.clearAllMocks(); list.mockResolvedValue({ items: [], total: 0 }) })

describe('Admin log navigation', () => {
  it('keeps the sidebar audit entry and switches to diagnostics using the URL', async () => {
    const { wrapper, router } = await mountLogs('/admin/audit-logs?keep=1')
    expect(list).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-testid="audit-table"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="debug-table"]').exists()).toBe(false)
    await wrapper.get('[data-testid="debug-log-tab"]').trigger('click')
    await flushPromises()
    expect(router.currentRoute.value.query).toEqual({ keep: '1', tab: 'debug' })
    expect(wrapper.find('[data-testid="audit-table"]').exists()).toBe(false)
    expect(wrapper.findComponent({ name: 'OpsSystemLogTable' }).props('diagnosticsMode')).toBe(true)
    expect(wrapper.get('[data-testid="debug-log-tab"]').attributes('aria-current')).toBe('page')
    await wrapper.get('[data-testid="audit-log-tab"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="debug-table"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="audit-table"]').exists()).toBe(true)
    expect(router.currentRoute.value.query).toEqual({ keep: '1' })
    expect(list).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('opens a bookmarked diagnostics URL without fetching audit logs and supports back', async () => {
    const { wrapper, router } = await mountLogs('/admin/audit-logs?tab=debug')
    expect(list).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="debug-table"]').exists()).toBe(true)
    await router.push('/admin/audit-logs')
    await flushPromises()
    expect(list).toHaveBeenCalledTimes(1)
    router.back()
    await new Promise(resolve => setTimeout(resolve, 0))
    await flushPromises()
    expect(wrapper.find('[data-testid="debug-table"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="audit-table"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('falls back to audit logs for unknown tabs', async () => {
    const { wrapper } = await mountLogs('/admin/audit-logs?tab=unknown')
    expect(wrapper.find('[data-testid="audit-table"]').exists()).toBe(true)
    expect(list).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })
})
