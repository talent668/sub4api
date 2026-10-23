// Never stringify an API extra object directly. These fixed-depth schemas are a
// second safety boundary after the server projection, including for old records.
export type DiagnosticJSONValue = string | number | boolean | null | DiagnosticJSONObject | DiagnosticJSONValue[]
export interface DiagnosticJSONObject { [key: string]: DiagnosticJSONValue }

type ScalarRule = (value: unknown) => DiagnosticJSONValue | undefined
interface SnapshotSchema { readonly [key: string]: ScalarRule | SnapshotSchema }

export const diagnosticSnapshotSections = [
  'client_request', 'upstream_request', 'client_request_headers',
  'upstream_request_headers', 'upstream_handshake_headers', 'response', 'usage'
] as const
export type DiagnosticSnapshotSection = typeof diagnosticSnapshotSections[number]
export const diagnosticUsageFields = [
  'input_tokens', 'output_tokens', 'cache_read_input_tokens',
  'cache_creation_input_tokens', 'reasoning_tokens', 'total_tokens'
] as const

const enumRule = (...allowed: string[]): ScalarRule => value =>
  typeof value === 'string' && allowed.includes(value) ? value : undefined
const booleanRule: ScalarRule = value => typeof value === 'boolean' ? value : undefined
const numberRule = (max: number, integer = false): ScalarRule => value =>
  typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= max && (!integer || Number.isInteger(value))
    ? value : undefined
const integerRule = numberRule(1_000_000_000, true)
const modelRule: ScalarRule = value => {
  if (typeof value !== 'string' || !/^[a-zA-Z0-9._:/-]{1,128}$/.test(value)) return undefined
  if (/(?:sk-|bearer|eyj|ghp_|gho_|xox|http:|https:|token|secret|password|credential|authorization|cookie|session|ticket|api_key|apikey)/i.test(value)) return undefined
  return value
}
const effortRule = enumRule('none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max')
const tierRule = enumRule('auto', 'default', 'flex', 'scale', 'priority', 'ultrafast')
const betaValues = ['responses_websockets=2026-02-04', 'responses_websockets=2026-02-06']
const betaRule: ScalarRule = value => {
  if (!Array.isArray(value) || value.length > 4) return undefined
  // Inspect at most four known entries, never recurse through arbitrary arrays.
  const safe: string[] = []
  for (let index = 0; index < value.length; index++) {
    const item = ownValue(value, String(index))
    if (typeof item === 'string' && betaValues.includes(item) && !safe.includes(item)) safe.push(item)
  }
  return safe.length ? safe : undefined
}
const headersSchema: SnapshotSchema = {
  content_type: enumRule('application/json', 'text/event-stream'),
  accept: enumRule('application/json', 'text/event-stream', '*/*'),
  anthropic_version: enumRule('2023-06-01'),
  openai_beta: betaRule,
  connection: enumRule('upgrade', 'keep-alive', 'close'),
  upgrade: enumRule('websocket')
}
const schemas: Record<DiagnosticSnapshotSection, SnapshotSchema> = {
  client_request: {
    model: modelRule,
    stream: booleanRule,
    max_tokens: integerRule,
    speed: enumRule('fast', 'standard', 'normal'),
    temperature: numberRule(2),
    top_p: numberRule(1),
    thinking: { type: enumRule('enabled', 'disabled', 'adaptive'), budget_tokens: integerRule },
    output_config: { effort: effortRule },
    tool_choice: enumRule('auto', 'any', 'tool', 'none'),
    messages_count: integerRule,
    tools_count: integerRule
  },
  upstream_request: {
    type: enumRule('response.create'),
    model: modelRule,
    stream: booleanRule,
    store: booleanRule,
    service_tier: tierRule,
    reasoning: { effort: effortRule, summary: enumRule('auto', 'concise', 'detailed') },
    max_output_tokens: integerRule,
    temperature: numberRule(2),
    top_p: numberRule(1),
    parallel_tool_calls: booleanRule,
    text: { verbosity: enumRule('low', 'medium', 'high'), format: { type: enumRule('text', 'json_object', 'json_schema') } },
    tool_choice: enumRule('auto', 'none', 'required'),
    input_count: integerRule,
    tools_count: integerRule,
    has_previous_response_id: booleanRule,
    has_prompt_cache_key: booleanRule,
    write_attempted: booleanRule,
    write_completed: booleanRule
  },
  client_request_headers: headersSchema,
  upstream_request_headers: headersSchema,
  upstream_handshake_headers: headersSchema,
  response: {
    model: modelRule,
    service_tier: tierRule,
    status: enumRule('queued', 'in_progress', 'completed', 'incomplete', 'failed', 'cancelled', 'canceled'),
    terminal_event: enumRule('response.completed', 'response.done', 'response.failed', 'response.incomplete', 'response.cancelled', 'response.canceled', 'error'),
    incomplete_reason: enumRule('max_output_tokens', 'content_filter'),
    error_code: enumRule(
      'server_error', 'internal_error', 'rate_limit_exceeded', 'insufficient_quota', 'invalid_api_key',
      'invalid_request_error', 'invalid_request', 'invalid_value', 'invalid_parameter', 'missing_required_parameter',
      'unknown_parameter', 'unsupported_parameter', 'model_not_found', 'context_length_exceeded', 'max_output_tokens',
      'content_filter', 'content_policy_violation', 'previous_response_not_found', 'invalid_encrypted_content',
      'websocket_connection_limit_reached', 'websocket_session_timeout', 'websocket_event_type_not_supported',
      'websocket_error', 'request_timeout', 'timeout', 'cancelled', 'canceled'
    ),
    error_type: enumRule(
      'invalid_request_error', 'authentication_error', 'permission_error', 'not_found_error', 'rate_limit_error',
      'rate_limit_exceeded', 'api_error', 'server_error', 'overloaded_error', 'timeout_error', 'connection_error'
    )
  },
  usage: {
    input_tokens: integerRule,
    output_tokens: integerRule,
    cache_read_input_tokens: integerRule,
    cache_creation_input_tokens: integerRule,
    reasoning_tokens: integerRule,
    total_tokens: integerRule
  }
}

// Data properties only: getters, inherited fields and toJSON hooks are not read.
function ownValue(source: object, key: string): unknown {
  const descriptor = Object.getOwnPropertyDescriptor(source, key)
  return descriptor && 'value' in descriptor ? descriptor.value : undefined
}
function isRecord(value: unknown): value is Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false
  const prototype = Object.getPrototypeOf(value)
  return prototype === Object.prototype || prototype === null
}
function project(schema: SnapshotSchema, input: unknown): DiagnosticJSONObject {
  const output: DiagnosticJSONObject = {}
  if (!isRecord(input)) return output
  // Only schema keys are traversed. The maximum nesting is text.format.type;
  // unknown keys/arrays/credentials never enter the rendered or copied object.
  for (const key of Object.keys(schema)) {
    const rule = schema[key]!
    const value = ownValue(input, key)
    if (typeof rule === 'function') {
      const safe = rule(value)
      if (safe !== undefined) output[key] = safe
    } else {
      const safe = project(rule, value)
      if (Object.keys(safe).length) output[key] = safe
    }
  }
  return output
}

export function projectDiagnosticSnapshot(section: DiagnosticSnapshotSection, input: unknown): DiagnosticJSONObject {
  if (!Object.prototype.hasOwnProperty.call(schemas, section)) return {}
  return project(schemas[section], input)
}

export function projectDiagnosticSnapshots(extra: unknown): Partial<Record<DiagnosticSnapshotSection, DiagnosticJSONObject>> {
  const output: Partial<Record<DiagnosticSnapshotSection, DiagnosticJSONObject>> = {}
  if (!isRecord(extra)) return output
  for (const section of diagnosticSnapshotSections) {
    const safe = projectDiagnosticSnapshot(section, ownValue(extra, section))
    if (Object.keys(safe).length) output[section] = safe
  }
  return output
}
