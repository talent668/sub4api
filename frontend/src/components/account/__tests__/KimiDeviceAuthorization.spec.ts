import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import KimiDeviceAuthorization from '../KimiDeviceAuthorization.vue'

const { startMock, pollMock } = vi.hoisted(() => ({ startMock: vi.fn(), pollMock: vi.fn() }))
vi.mock('@/api/admin/kimi', () => ({ kimiOAuthAPI: { start: startMock, poll: pollMock } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const grant = { session_id: 'session', user_code: 'ABCD-EFGH', verification_uri: 'https://auth.kimi.com/device', expires_in: 600, interval: 5 }

describe('Kimi device authorization', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    startMock.mockReset().mockResolvedValue(grant)
    pollMock.mockReset().mockResolvedValue({ status: 'pending', interval: 5 })
  })
  afterEach(() => vi.useRealTimers())

  it('polls at the server interval and emits credentials once after authorization', async () => {
    const credentials = { access_token: 'secret-access', refresh_token: 'secret-refresh', device_id: 'device' }
    pollMock.mockResolvedValueOnce({ status: 'pending', interval: 10 }).mockResolvedValueOnce({ status: 'authorized', credentials })
    const wrapper = mount(KimiDeviceAuthorization, { props: { proxyId: 7 } })
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(startMock.mock.calls[0]?.[0]).toBe(7)
    expect(wrapper.get('a').attributes('href')).toBe(grant.verification_uri)
    expect(wrapper.text()).toContain('ABCD-EFGH')
    await vi.advanceTimersByTimeAsync(5000)
    await flushPromises()
    expect(pollMock).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(9999)
    expect(pollMock).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    await flushPromises()
    expect(wrapper.emitted('authorized')).toEqual([[credentials]])
    expect(wrapper.html()).not.toContain('secret-access')
    expect(wrapper.html()).not.toContain('secret-refresh')
    await vi.advanceTimersByTimeAsync(60000)
    expect(pollMock).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('aborts and ignores a late successful poll after the dialog is closed', async () => {
    let resolvePoll: ((value: unknown) => void) | undefined
    pollMock.mockImplementation(() => new Promise((resolve) => { resolvePoll = resolve }))
    const wrapper = mount(KimiDeviceAuthorization)
    await wrapper.get('button').trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(5000)
    const signal = pollMock.mock.calls[0]?.[1] as AbortSignal
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
    resolvePoll?.({ status: 'authorized', credentials: { access_token: 'late' } })
    await flushPromises()
    expect(wrapper.emitted('authorized')).toBeUndefined()
  })

  it('resets a flow when its proxy changes and stops expired polling', async () => {
    startMock.mockResolvedValueOnce({ ...grant, expires_in: 5 })
    const wrapper = mount(KimiDeviceAuthorization, { props: { proxyId: 7 } })
    await wrapper.get('button').trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(5000)
    expect(pollMock).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('expired')
    await wrapper.setProps({ proxyId: 8 })
    expect(wrapper.find('a').exists()).toBe(false)
    wrapper.unmount()
  })
})
