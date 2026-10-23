import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import type { OpsSystemLog } from '@/api/admin/ops'
import enLocale from '@/i18n/locales/en'
import zhLocale from '@/i18n/locales/zh'
import RequestDiagnosticDetails from '../RequestDiagnosticDetails.vue'
import DiagnosticJsonBlock from '../DiagnosticJsonBlock.vue'
import { diagnosticSnapshotSections, diagnosticUsageFields } from '../../utils/diagnosticSnapshot'

const { copyToClipboard } = vi.hoisted(() => ({ copyToClipboard: vi.fn().mockResolvedValue(true) }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard }) }))
// Vite aliases vue-i18n to its runtime-only build. Resolve this suite's static
// English copy directly so assertions do not depend on a message compiler.
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({
    t: (key: string) => {
      let value: unknown = enLocale
      for (const part of key.split('.')) {
        if (!value || typeof value !== 'object') return key
        value = (value as Record<string, unknown>)[part]
      }
      return typeof value === 'string' ? value : key
    }
  })
}))

function mountDetails(extra?: Record<string, unknown>, summary = false) {
  const row: OpsSystemLog = {
    id: 1, created_at: '2026-10-23T10:00:00Z', host: 'gateway', level: 'debug', component: 'gateway.request_diagnostics',
    message: 'openai.messages_ws_latency', request_id: 'request-safe', extra
  }
  return mount(RequestDiagnosticDetails, { props: { row, summary } })
}

describe('request diagnostic metadata details', () => {
  beforeEach(() => vi.clearAllMocks())

  it('starts every metadata section collapsed and keeps summary rows unchanged', () => {
    const extra = { prep_ms: 0, conn_reused: false, client_request: { model: 'gpt-5.4', stream: false } }
    const details = mountDetails(extra)
    expect(details.findAll('details')).toHaveLength(7)
    for (const element of details.findAll('details')) expect((element.element as HTMLDetailsElement).open).toBe(false)
    expect(details.text()).toContain('0 ms')
    expect(details.text()).toContain('false')
    const summary = mountDetails(extra, true)
    expect(summary.findAll('details')).toHaveLength(0)
    expect(summary.find('[data-testid="diagnostic-snapshots"]').exists()).toBe(false)
    expect(summary.text()).not.toContain('Client request parameters')
    details.unmount()
    summary.unmount()
  })

  it('highlights projected JSON keys, strings, numbers, booleans and punctuation using text spans', async () => {
    const wrapper = mountDetails({ client_request: { model: 'gpt-5.4', stream: false, max_tokens: 0, thinking: { type: 'enabled', budget_tokens: 0 } } })
    const block = wrapper.get('[data-testid="diagnostic-section-client_request"]')
    await block.get('summary').trigger('click')
    const json = block.get('pre')
    expect(json.find('.diagnostic-json-key').text()).toBe('"model"')
    expect(json.find('.diagnostic-json-string').text()).toBe('"gpt-5.4"')
    expect(json.find('.diagnostic-json-number').text()).toBe('0')
    expect(json.find('.diagnostic-json-boolean').text()).toBe('false')
    expect(json.find('.diagnostic-json-punctuation').text()).toBe('{')
    expect(JSON.parse(json.text())).toEqual({ model: 'gpt-5.4', stream: false, max_tokens: 0, thinking: { type: 'enabled', budget_tokens: 0 } })
    wrapper.unmount()
  })

  it('never renders malicious HTML, unknown nested content, credentials or raw row data; copies exactly one safe section', async () => {
    const malicious = '<img src=x onerror="alert(1)"><script>window.secret=true</script>'
    const wrapper = mountDetails({
      client_request: {
        model: 'gpt-5.4', stream: false, temperature: 0,
        thinking: { type: 'enabled', budget_tokens: 0, content: malicious, authorization: 'PRIVATE_NESTED' },
        output_config: { effort: 'high', password: 'PRIVATE_PASSWORD' }, messages: [{ content: malicious }],
        [malicious]: 'PRIVATE_UNKNOWN_KEY'
      },
      upstream_request: { model: malicious, store: false, text: { format: { type: 'json_schema', schema: { token: 'PRIVATE_SCHEMA' } } } },
      client_request_headers: { content_type: 'application/json', authorization: 'Bearer PRIVATE_AUTH', cookie: 'PRIVATE_COOKIE' },
      response: { error_code: malicious, error_type: 'PRIVATE_ERROR', output: malicious },
      authorization: 'PRIVATE_ROOT', response_body: malicious
    })
    expect(wrapper.findAll('img, script, iframe')).toHaveLength(0)
    expect(wrapper.html()).not.toContain('<img')
    expect(wrapper.text()).not.toContain('PRIVATE')
    expect(wrapper.text()).not.toContain('window.secret')
    await wrapper.get('[data-testid="copy-diagnostic-client_request"]').trigger('click')
    const expected = JSON.stringify({
      model: 'gpt-5.4', stream: false, temperature: 0,
      thinking: { type: 'enabled', budget_tokens: 0 }, output_config: { effort: 'high' }
    }, null, 2)
    expect(copyToClipboard).toHaveBeenCalledTimes(1)
    expect(copyToClipboard).toHaveBeenCalledWith(expected, 'Safe metadata JSON copied')
    expect(copyToClipboard.mock.calls[0]![0]).not.toContain('request-safe')
    expect(copyToClipboard.mock.calls[0]![0]).not.toContain('client_request')
    expect(copyToClipboard.mock.calls[0]![0]).not.toContain('PRIVATE')
    wrapper.unmount()
  })

  it('escapes text props rather than interpreting HTML even when a block receives hostile strings', () => {
    const malicious = '<img src=x onerror="alert(1)">'
    const wrapper = mount(DiagnosticJsonBlock, {
      props: { section: 'client_request', value: { model: malicious }, title: malicious, hint: malicious }
    })
    expect(wrapper.findAll('img')).toHaveLength(0)
    expect(wrapper.html()).toContain('&lt;img')
    expect(wrapper.text()).toContain(malicious)
    expect(wrapper.find('pre').exists()).toBe(false)
    expect(wrapper.find('button').exists()).toBe(false)
    wrapper.unmount()
  })

  it('labels older missing snapshots as not recorded and missing usage as not reported, never zero', () => {
    const wrapper = mountDetails({ prep_ms: 0 })
    for (const section of diagnosticSnapshotSections) {
      const block = wrapper.get(`[data-testid="diagnostic-section-${section}"]`)
      expect(block.get('[data-testid="diagnostic-snapshot-missing"]').text()).toBe(section === 'usage' ? 'Not reported' : 'Not recorded')
      expect(block.find('button').exists()).toBe(false)
      expect(block.find('pre').exists()).toBe(false)
    }
    wrapper.unmount()
  })

  it('keeps reported zero and reasoning tokens without summing or inventing totals', () => {
    const wrapper = mountDetails({ usage: { input_tokens: 0, output_tokens: 12, reasoning_tokens: 8 } })
    const block = wrapper.get('[data-testid="diagnostic-section-usage"]')
    expect(JSON.parse(block.get('pre').text())).toEqual({ input_tokens: 0, output_tokens: 12, reasoning_tokens: 8 })
    const missing = block.get('[data-testid="diagnostic-unreported-usage"]')
    expect(missing.text()).toContain('Total tokens')
    expect(missing.text()).toContain('Not reported')
    expect(missing.text()).not.toContain('Output tokens')
    expect(block.text()).toContain('these fields are not added together')
    wrapper.unmount()
  })

  it('keeps requested and returned model/tier separate and explains proposed versus cached handshake scope', () => {
    const wrapper = mountDetails({
      upstream_request: { model: 'gpt-5.4', service_tier: 'priority', write_attempted: true, write_completed: false },
      response: { model: 'gpt-5.4-2026-03-05', service_tier: 'default' },
      upstream_request_headers: { connection: 'upgrade', upgrade: 'websocket' },
      upstream_handshake_headers: { connection: 'upgrade', upgrade: 'websocket' }
    })
    expect(wrapper.text()).toContain('not what actually ran')
    expect(wrapper.text()).toContain('does not send a new handshake for every turn')
    expect(wrapper.text()).toContain('not per-turn response headers')
    expect(wrapper.text()).toContain('do not prove delivery')
    const requested = JSON.parse(wrapper.get('[data-testid="diagnostic-json-upstream_request"]').text())
    const actual = JSON.parse(wrapper.get('[data-testid="diagnostic-json-response"]').text())
    expect(requested).toEqual({ model: 'gpt-5.4', service_tier: 'priority', write_attempted: true, write_completed: false })
    expect(actual).toEqual({ model: 'gpt-5.4-2026-03-05', service_tier: 'default' })
    wrapper.unmount()
  })

  it.each([['en', enLocale], ['zh', zhLocale]] as const)('provides complete %s metadata copy and section/usage labels', (_language, locale) => {
    const strings = locale.admin.ops.systemLogs.snapshots
    for (const key of ['title', 'safetyHint', 'requestedActualHint', 'writeHint', 'proposedHeadersHint', 'handshakeHint', 'usageHint', 'notReported', 'copy', 'copied'] as const) {
      expect(strings[key]).toBeTruthy()
    }
    for (const section of diagnosticSnapshotSections) expect(strings.sections[section]).toBeTruthy()
    for (const field of diagnosticUsageFields) expect(strings.usageFields[field]).toBeTruthy()
    expect(Object.keys(enLocale.admin.ops.systemLogs.snapshots).sort()).toEqual(Object.keys(zhLocale.admin.ops.systemLogs.snapshots).sort())
  })
})
