import { describe, expect, it } from 'vitest'

import { durationSeverity, firstTokenSeverity, formatUsageOutputRate, formatUsageSessionOutputRate, formatUsageCacheRate } from '../latencyHealth'

describe('latencyHealth', () => {
  it('classifies first-token latency at 10s/30s/60s boundaries', () => {
    expect(firstTokenSeverity(0)).toBe('good')
    expect(firstTokenSeverity(9_999)).toBe('good')
    expect(firstTokenSeverity(10_000)).toBe('warn')
    expect(firstTokenSeverity(29_999)).toBe('warn')
    expect(firstTokenSeverity(30_000)).toBe('slow')
    expect(firstTokenSeverity(59_999)).toBe('slow')
    expect(firstTokenSeverity(60_000)).toBe('critical')
  })

  it('classifies total duration at 1min/3min/5min boundaries', () => {
    expect(durationSeverity(0)).toBe('good')
    expect(durationSeverity(59_999)).toBe('good')
    expect(durationSeverity(60_000)).toBe('warn')
    expect(durationSeverity(179_999)).toBe('warn')
    expect(durationSeverity(180_000)).toBe('slow')
    expect(durationSeverity(299_999)).toBe('slow')
    expect(durationSeverity(300_000)).toBe('critical')
  })
})

describe('formatUsageOutputRate', () => {
  const row = {
    output_tokens: 1000,
    duration_ms: 20_000,
    image_count: 0,
    image_output_tokens: 0,
    billing_mode: 'token',
  }

  it.each(['sync', 'stream', 'ws_v2', 'cyber', undefined] as const)(
    'uses total duration for %s requests', (request_type) => {
      expect(formatUsageOutputRate({ ...row, request_type })).toBe('50.0 tok/s')
    }
  )

  it('rounds to one decimal place', () => {
    expect(formatUsageOutputRate({ ...row, output_tokens: 101, duration_ms: 3000 })).toBe('33.7 tok/s')
  })

  it.each([null, 0, -1, NaN, Infinity])('does not invent a rate for duration %s', (duration_ms) => {
    expect(formatUsageOutputRate({ ...row, duration_ms })).toBe('—')
  })

  it.each([0, -1, NaN, Infinity])('does not invent a rate for output %s', (output_tokens) => {
    expect(formatUsageOutputRate({ ...row, output_tokens })).toBe('—')
  })

  it.each([
    { image_count: 1 },
    { image_output_tokens: 100 },
    { billing_mode: 'image' },
    { request_type: 'live' as const },
    { request_type: 'unknown' as const },
  ])('omits non-text or unknown request rates: %j', (fields) => {
    expect(formatUsageOutputRate({ ...row, ...fields })).toBe('—')
  })
})

describe('formatUsageSessionOutputRate', () => {
  const row = { session_id: 'conversation-1', session_average_output_tps: 47.36, session_tps_request_count: 3 }

  it('shows the backend session rate without recomputing from the current page', () => {
    expect(formatUsageSessionOutputRate(row)).toBe('47.4 tok/s')
  })

  it.each([
    { session_id: undefined }, { session_id: ' ' },
    { session_tps_request_count: 0 }, { session_tps_request_count: undefined },
    { session_average_output_tps: null }, { session_average_output_tps: undefined },
    { session_average_output_tps: NaN }, { session_average_output_tps: Infinity },
    { session_average_output_tps: 0 }, { session_average_output_tps: -1 },
  ])('keeps unavailable session rates distinct from zero: %j', (fields) => {
    expect(formatUsageSessionOutputRate({ ...row, ...fields })).toBe('—')
  })
})

describe('formatUsageCacheRate', () => {
  const row = { input_tokens: 100, cache_creation_tokens: 200, cache_read_tokens: 9700, image_count: 0, image_output_tokens: 0, billing_mode: 'token' }

  it('includes cache creation in input and excludes output from the denominator', () => {
    expect(formatUsageCacheRate(row)).toBe('97.0%')
  })

  it('shows uncached and fully cached input as 0% and 100%', () => {
    expect(formatUsageCacheRate({ ...row, cache_read_tokens: 0 })).toBe('0.0%')
    expect(formatUsageCacheRate({ ...row, input_tokens: 0, cache_creation_tokens: 0 })).toBe('100.0%')
    expect(formatUsageCacheRate({ ...row, input_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0 })).toBe('—')
  })

  it.each([
    { input_tokens: -1 }, { input_tokens: NaN }, { cache_creation_tokens: -1 },
    { cache_read_tokens: Infinity }, { image_count: 1 }, { image_output_tokens: 100 }, { billing_mode: 'image' },
  ])('omits invalid or inapplicable ratios: %j', (fields) => {
    expect(formatUsageCacheRate({ ...row, ...fields })).toBe('—')
  })
})
