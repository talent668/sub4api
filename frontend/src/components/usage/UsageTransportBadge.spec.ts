import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import UsageTransportBadge from './UsageTransportBadge.vue'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
describe('actual transport route', () => {
  it('keeps absent historical routes unknown', () => {
    const wrapper = mount(UsageTransportBadge)
    expect(wrapper.text()).toBe('usage.transportNotRecorded')
    expect(wrapper.attributes('title')).toBe('usage.transportNotRecordedHint')
  })
  it('shows both protocol directions and a bounded fallback reason', () => {
    const wrapper = mount(UsageTransportBadge, { props: { details: { client: 'ws', upstream: 'http', mode: 'http_bridge', fallback_reason: 'large_payload' } } })
    expect(wrapper.text()).toBe('WS→HTTP')
    expect(wrapper.attributes('title')).toContain('usage.transportFallbackReasons.large_payload')
  })

  it('shows a verified explicit association without changing the actual HTTP route', () => {
    const attempt = 'aa7c0b90-fdf6-4cbb-89a5-9ee9b53f2d39'
    const wrapper = mount(UsageTransportBadge, { props: { details: { client: 'http', upstream: 'http', client_ws_failure: { attempt_id: attempt, evidence: 'receipt_v1' } } } })
    expect(wrapper.text()).toContain('HTTP→HTTP')
    expect(wrapper.get('[data-testid="client-ws-failure"]').text()).toBe('usage.clientWSFailureLinked')
    expect(wrapper.attributes('title')).toContain('usage.clientWSFailureLinkedHint')
    expect(wrapper.attributes('title')).not.toContain('usage.transportFallbackReasons.ws_failure')
  })

  it.each([
    { client: 'http', upstream: 'http', client_ws_failure: { attempt_id: 'private-token', evidence: 'receipt_v1' } },
    { client: 'http', upstream: 'http', client_ws_failure: { attempt_id: 'aa7c0b90-fdf6-4cbb-89a5-9ee9b53f2d39', evidence: 'client_claim' } },
    { client: 'ws', upstream: 'ws', client_ws_failure: { attempt_id: 'aa7c0b90-fdf6-4cbb-89a5-9ee9b53f2d39', evidence: 'receipt_v1' } },
  ])('does not display unverified or incoherent client failure data', details => {
    const wrapper = mount(UsageTransportBadge, { props: { details: details as any } })
    expect(wrapper.find('[data-testid="client-ws-failure"]').exists()).toBe(false)
    expect(wrapper.attributes('title')).not.toContain('private-token')
  })
})
