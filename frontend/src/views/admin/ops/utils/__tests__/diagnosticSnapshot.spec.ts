import { describe, expect, it, vi } from 'vitest'
import { diagnosticSnapshotSections, projectDiagnosticSnapshot, projectDiagnosticSnapshots } from '../diagnosticSnapshot'

describe('content-free diagnostic snapshot projection', () => {
  it('keeps only approved parameters at a fixed depth, preserving zero and false', () => {
    const source = {
      client_request: {
        model: 'claude-sonnet-4-5', stream: false, max_tokens: 0, speed: 'fast', temperature: 0, top_p: 0,
        thinking: { type: 'enabled', budget_tokens: 0, content: 'PRIVATE_THINKING' },
        output_config: { effort: 'high', schema: { secret: 'PRIVATE_SCHEMA' } },
        tool_choice: 'tool', messages_count: 0, tools_count: 0,
        messages: [{ content: 'PRIVATE_PROMPT' }], tools: [{ name: 'PRIVATE_TOOL' }]
      },
      upstream_request: {
        type: 'response.create', model: 'gpt-5.4', stream: true, store: false, service_tier: 'priority',
        reasoning: { effort: 'xhigh', summary: 'auto', content: 'PRIVATE_REASONING' },
        max_output_tokens: 0, temperature: 0, top_p: 0, parallel_tool_calls: false,
        text: { verbosity: 'low', format: { type: 'json_schema', schema: { password: 'PRIVATE_SCHEMA' } }, output: 'PRIVATE_OUTPUT' },
        tool_choice: 'required', input_count: 0, tools_count: 0, has_previous_response_id: false,
        has_prompt_cache_key: true, write_attempted: true, write_completed: false,
        previous_response_id: 'PRIVATE_RESPONSE_ID', prompt_cache_key: 'PRIVATE_CACHE_KEY', input: [{ content: 'PRIVATE_INPUT' }]
      },
      secret: 'PRIVATE_ROOT'
    }
    expect(projectDiagnosticSnapshots(source)).toEqual({
      client_request: {
        model: 'claude-sonnet-4-5', stream: false, max_tokens: 0, speed: 'fast', temperature: 0, top_p: 0,
        thinking: { type: 'enabled', budget_tokens: 0 }, output_config: { effort: 'high' }, tool_choice: 'tool', messages_count: 0, tools_count: 0
      },
      upstream_request: {
        type: 'response.create', model: 'gpt-5.4', stream: true, store: false, service_tier: 'priority',
        reasoning: { effort: 'xhigh', summary: 'auto' }, max_output_tokens: 0, temperature: 0, top_p: 0,
        parallel_tool_calls: false, text: { verbosity: 'low', format: { type: 'json_schema' } }, tool_choice: 'required',
        input_count: 0, tools_count: 0, has_previous_response_id: false, has_prompt_cache_key: true,
        write_attempted: true, write_completed: false
      }
    })
    expect(JSON.stringify(projectDiagnosticSnapshots(source))).not.toContain('PRIVATE')
  })

  it('accepts only known canonical header keys and header values, not credentials or arbitrary beta flags', () => {
    const headers = {
      content_type: 'application/json', accept: 'text/event-stream', anthropic_version: '2023-06-01',
      openai_beta: ['responses_websockets=2026-02-04', 'responses_websockets=2026-02-06', 'SECRET_BETA', 'responses_websockets=2026-02-04'],
      connection: 'upgrade', upgrade: 'websocket', authorization: 'Bearer SECRET', cookie: 'SECRET_COOKIE',
      'x-api-key': 'SECRET_API_KEY', user_agent: 'PRIVATE_USER_AGENT', 'Content-Type': 'text/html', location: 'https://secret.example'
    }
    const expected = {
      content_type: 'application/json', accept: 'text/event-stream', anthropic_version: '2023-06-01',
      openai_beta: ['responses_websockets=2026-02-04', 'responses_websockets=2026-02-06'], connection: 'upgrade', upgrade: 'websocket'
    }
    for (const section of ['client_request_headers', 'upstream_request_headers', 'upstream_handshake_headers'] as const) {
      expect(projectDiagnosticSnapshot(section, headers)).toEqual(expected)
    }
    expect(projectDiagnosticSnapshot('client_request_headers', {
      content_type: 'application/json; bearer=SECRET', accept: '<img src=x onerror=alert(1)>',
      anthropic_version: '2023-06-01-secret', openai_beta: Array(5).fill('responses_websockets=2026-02-04'),
      connection: 'Upgrade, SECRET', upgrade: 'websocket-secret'
    })).toEqual({})
  })

  it('rejects arbitrary enum strings, HTML, labels containing credentials and incorrect types', () => {
    for (const model of ['<img src=x onerror=alert(1)>', 'x'.repeat(129), 'gpt/sk-test', 'gpt-bearerABC', 'my_secret_model', 'gpt-token-123', 'https://example.com']) {
      expect(projectDiagnosticSnapshot('client_request', { model })).toEqual({})
    }
    expect(projectDiagnosticSnapshots({
      client_request: { stream: 0, speed: 'SUPER_SECRET', thinking: { type: '<script>alert(1)</script>' }, output_config: { effort: 'SECRET' }, tool_choice: { type: 'tool', name: 'SECRET' } },
      upstream_request: { store: 'false', service_tier: 'PRIVATE_TIER', reasoning: { effort: 'PRIVATE_REASONING', summary: 'secret' }, text: { format: { type: 'secret' } } },
      response: { status: 'SECRET_STATUS', terminal_event: 'SECRET_EVENT', error_code: 'SECRET_ERROR', error_type: 'Bearer secret' },
      usage: { input_tokens: '0', output_tokens: false, total_tokens: null }
    })).toEqual({})
  })

  it('bounds every numeric field without coercing, defaulting or deriving token totals', () => {
    for (const bad of [-1, 1.1, Number.NaN, Infinity, 1_000_000_001, '12', null]) {
      expect(projectDiagnosticSnapshot('usage', { input_tokens: bad })).toEqual({})
    }
    expect(projectDiagnosticSnapshot('usage', { input_tokens: 0, output_tokens: 1_000_000_000, reasoning_tokens: 5 })).toEqual({
      input_tokens: 0, output_tokens: 1_000_000_000, reasoning_tokens: 5
    })
    expect(projectDiagnosticSnapshot('client_request', { temperature: 2.01, top_p: 1.01, messages_count: 0.5 })).toEqual({})
    expect(projectDiagnosticSnapshot('client_request', { temperature: 0.25, top_p: 0.75 })).toEqual({ temperature: 0.25, top_p: 0.75 })
  })

  it('projects returned model/tier/status separately without inventing response or usage fields', () => {
    expect(projectDiagnosticSnapshots({
      upstream_request: { model: 'gpt-5.4', service_tier: 'priority' },
      response: { model: 'gpt-5.4-2026-03-05', service_tier: 'default', status: 'incomplete', terminal_event: 'response.incomplete', incomplete_reason: 'max_output_tokens', error_code: 'request_timeout', error_type: 'timeout_error', output: 'PRIVATE' }
    })).toEqual({
      upstream_request: { model: 'gpt-5.4', service_tier: 'priority' },
      response: { model: 'gpt-5.4-2026-03-05', service_tier: 'default', status: 'incomplete', terminal_event: 'response.incomplete', incomplete_reason: 'max_output_tokens', error_code: 'request_timeout', error_type: 'timeout_error' }
    })
  })

  it('fails closed for old records, arrays, inherited data, accessors, cycles and toJSON hooks', () => {
    for (const old of [undefined, null, [], 'secret', { prep_ms: 0 }]) expect(projectDiagnosticSnapshots(old)).toEqual({})
    const getter = vi.fn(() => 'SECRET')
    const toJSON = vi.fn(() => ({ secret: 'SECRET' }))
    const input = { stream: false, text: { format: { type: 'text' } }, toJSON } as Record<string, unknown>
    Object.defineProperty(input, 'model', { enumerable: true, get: getter })
    input.unknown = input
    expect(projectDiagnosticSnapshot('upstream_request', input)).toEqual({ stream: false, text: { format: { type: 'text' } } })
    expect(getter).not.toHaveBeenCalled()
    expect(toJSON).not.toHaveBeenCalled()
    expect(projectDiagnosticSnapshots(Object.create({ client_request: { stream: true } }))).toEqual({})
    const polluted = JSON.parse('{"client_request":{"stream":false,"__proto__":{"password":"SECRET"}},"__proto__":{"client_request":{"model":"SECRET"}}}')
    expect(projectDiagnosticSnapshots(polluted)).toEqual({ client_request: { stream: false } })
  })

  it('detaches arrays and bounds output independently of unknown input size', () => {
    const beta = ['responses_websockets=2026-02-04']
    const nested: Record<string, unknown> = { model: 'gpt-5.4', stream: false }
    for (let index = 0; index < 5_000; index++) nested[`private_${index}`] = { body: 'PRIVATE'.repeat(100) }
    const result = projectDiagnosticSnapshots({ client_request: nested, client_request_headers: { openai_beta: beta } })
    beta.push('PRIVATE')
    nested.model = 'PRIVATE'
    expect(result).toEqual({ client_request: { model: 'gpt-5.4', stream: false }, client_request_headers: { openai_beta: ['responses_websockets=2026-02-04'] } })
    expect(JSON.stringify(result).length).toBeLessThan(200)
  })

  it('defines exactly the seven optional metadata sections', () => {
    expect(diagnosticSnapshotSections).toEqual(['client_request', 'upstream_request', 'client_request_headers', 'upstream_request_headers', 'upstream_handshake_headers', 'response', 'usage'])
  })
})
