import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import OpenAIFastBridgeSettings from '../OpenAIFastBridgeSettings.vue'
import type { OpenAIFastBridgeSettings as BridgeSettings } from '@/api/admin/settings'

const { get, put, showSuccess, showError } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn(),
}))

vi.mock('@/api/client', () => ({ apiClient: { get, put } }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showSuccess, showError }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const endpoint = '/admin/settings/openai-fast-bridge'
const toggle = '[data-testid="openai-fast-bridge-toggle"]'
const scope = '#openai-fast-bridge-scope'
const save = '[data-testid="openai-fast-bridge-save"]'

function mountSettings() {
  return mount(OpenAIFastBridgeSettings, {
    global: { stubs: { teleport: true } },
  })
}

type SettingsWrapper = ReturnType<typeof mountSettings>

async function selectScope(wrapper: SettingsWrapper, value: BridgeSettings['scope']) {
  await wrapper.get(scope).trigger('click')
  const key = value === 'all' ? 'scopeAll' : 'scopeFastOnly'
  const option = wrapper.findAll('[role="option"]').find(option =>
    option.text() === `admin.settings.openaiFastBridge.${key}`,
  )
  expect(option).toBeDefined()
  await option!.trigger('click')
}

describe('OpenAIFastBridgeSettings', () => {
  const wrappers: SettingsWrapper[] = []

  async function render() {
    const wrapper = mountSettings()
    wrappers.push(wrapper)
    await flushPromises()
    return wrapper
  }

  beforeEach(() => {
    vi.resetAllMocks()
    get.mockResolvedValue({ data: { enabled: false, scope: 'all' } })
    put.mockImplementation(async (_path, settings) => ({ data: { ...settings } }))
  })

  afterEach(() => {
    wrappers.splice(0).forEach(wrapper => wrapper.unmount())
  })

  it('defaults to all without enabling the bridge or automatically saving', async () => {
    const wrapper = await render()

    expect(get).toHaveBeenCalledWith(endpoint)
    expect(wrapper.get(toggle).attributes('aria-checked')).toBe('false')
    expect(wrapper.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeAll')
    expect(wrapper.get(scope).attributes('disabled')).toBeDefined()
    expect(put).not.toHaveBeenCalled()

    await wrapper.get(save).trigger('click')
    await flushPromises()
    expect(put).toHaveBeenCalledWith(endpoint, { enabled: false, scope: 'all' })
  })

  it.each([false, true])('loads a legacy response as all and preserves enabled=%s', async enabled => {
    get.mockResolvedValueOnce({ data: { enabled } })
    const wrapper = await render()

    expect(wrapper.get(toggle).attributes('aria-checked')).toBe(String(enabled))
    expect(wrapper.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeAll')
    await wrapper.get(save).trigger('click')
    await flushPromises()
    expect(put).toHaveBeenCalledWith(endpoint, { enabled, scope: 'all' })
  })

  it('switches all to fast_only and back using the same PUT endpoint', async () => {
    get.mockResolvedValueOnce({ data: { enabled: true, scope: 'all' } })
    const wrapper = await render()

    await selectScope(wrapper, 'fast_only')
    await wrapper.get(save).trigger('click')
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith(endpoint, { enabled: true, scope: 'fast_only' })
    expect(showSuccess).toHaveBeenCalledWith('admin.settings.openaiFastBridge.saved')

    await selectScope(wrapper, 'all')
    await wrapper.get(save).trigger('click')
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith(endpoint, { enabled: true, scope: 'all' })
  })

  it('preserves fast_only while disabled and when re-enabled', async () => {
    get.mockResolvedValueOnce({ data: { enabled: true, scope: 'fast_only' } })
    const wrapper = await render()

    await wrapper.get(toggle).trigger('click')
    expect(wrapper.get(scope).attributes('disabled')).toBeDefined()
    expect(wrapper.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeFastOnly')
    await wrapper.get(save).trigger('click')
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith(endpoint, { enabled: false, scope: 'fast_only' })

    await wrapper.get(toggle).trigger('click')
    expect(wrapper.get(scope).attributes('disabled')).toBeUndefined()
    expect(wrapper.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeFastOnly')
    await wrapper.get(save).trigger('click')
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith(endpoint, { enabled: true, scope: 'fast_only' })
  })

  it('hydrates both enabled and scope from the save response', async () => {
    get.mockResolvedValueOnce({ data: { enabled: true, scope: 'all' } })
    put.mockResolvedValueOnce({ data: { enabled: false, scope: 'fast_only' } })
    const wrapper = await render()

    await wrapper.get(save).trigger('click')
    await flushPromises()

    expect(wrapper.get(toggle).attributes('aria-checked')).toBe('false')
    expect(wrapper.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeFastOnly')
    expect(wrapper.get(scope).attributes('disabled')).toBeDefined()
  })

  it('defaults a legacy save response to all', async () => {
    get.mockResolvedValueOnce({ data: { enabled: true, scope: 'fast_only' } })
    put.mockResolvedValueOnce({ data: { enabled: true } })
    const wrapper = await render()

    await wrapper.get(save).trigger('click')
    await flushPromises()

    expect(wrapper.get(toggle).attributes('aria-checked')).toBe('true')
    expect(wrapper.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeAll')
  })

  it('reloads the saved enabled and scope values', async () => {
    let stored: BridgeSettings = { enabled: false, scope: 'all' }
    get.mockImplementation(async () => ({ data: { ...stored } }))
    put.mockImplementation(async (_path, settings: BridgeSettings) => {
      stored = { ...settings }
      return { data: { ...stored } }
    })
    const wrapper = await render()
    await wrapper.get(toggle).trigger('click')
    await selectScope(wrapper, 'fast_only')
    await wrapper.get(save).trigger('click')
    await flushPromises()
    wrapper.unmount()
    wrappers.splice(wrappers.indexOf(wrapper), 1)

    const reloaded = await render()
    expect(reloaded.get(toggle).attributes('aria-checked')).toBe('true')
    expect(reloaded.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeFastOnly')
    expect(get).toHaveBeenCalledTimes(2)
  })

  it('disables editing and repeated saves until the save response arrives', async () => {
    get.mockResolvedValueOnce({ data: { enabled: true, scope: 'all' } })
    let resolveSave!: (response: { data: BridgeSettings }) => void
    put.mockReturnValueOnce(new Promise(resolve => { resolveSave = resolve }))
    const wrapper = await render()

    await wrapper.get(save).trigger('click')
    expect(wrapper.get(save).attributes('disabled')).toBeDefined()
    expect(wrapper.get(toggle).attributes('disabled')).toBeDefined()
    expect(wrapper.get(scope).attributes('disabled')).toBeDefined()
    await wrapper.get(save).trigger('click')
    expect(put).toHaveBeenCalledTimes(1)

    resolveSave({ data: { enabled: true, scope: 'all' } })
    await flushPromises()
    expect(wrapper.get(save).attributes('disabled')).toBeUndefined()
    expect(wrapper.get(toggle).attributes('disabled')).toBeUndefined()
    expect(wrapper.get(scope).attributes('disabled')).toBeUndefined()
  })

  it.each([
    [new Error('load unavailable'), 'load unavailable'],
    [{}, 'admin.settings.openaiFastBridge.loadFailed'],
  ])('reports load failures and keeps disabled/all defaults (%s)', async (error, message) => {
    get.mockRejectedValueOnce(error)
    const wrapper = await render()

    expect(showError).toHaveBeenCalledWith(message)
    expect(wrapper.get(toggle).attributes('aria-checked')).toBe('false')
    expect(wrapper.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeAll')
    expect(put).not.toHaveBeenCalled()
  })

  it.each([
    [new Error('save unavailable'), 'save unavailable'],
    [{}, 'admin.settings.openaiFastBridge.saveFailed'],
  ])('reports save failures without losing the edited scope (%s)', async (error, message) => {
    get.mockResolvedValueOnce({ data: { enabled: true, scope: 'all' } })
    put.mockRejectedValueOnce(error)
    const wrapper = await render()
    await selectScope(wrapper, 'fast_only')
    await wrapper.get(save).trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith(message)
    expect(showSuccess).not.toHaveBeenCalled()
    expect(wrapper.get(toggle).attributes('aria-checked')).toBe('true')
    expect(wrapper.get(scope).text()).toContain('admin.settings.openaiFastBridge.scopeFastOnly')
    expect(wrapper.get(save).attributes('disabled')).toBeUndefined()

    await wrapper.get(save).trigger('click')
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith(endpoint, { enabled: true, scope: 'fast_only' })
    expect(showSuccess).toHaveBeenCalledWith('admin.settings.openaiFastBridge.saved')
  })
})
