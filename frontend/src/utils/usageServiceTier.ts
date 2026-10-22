export function normalizeUsageServiceTier(serviceTier?: string | null): string | null {
  const value = serviceTier?.trim().toLowerCase()
  if (!value) return null
  if (value === 'fast') return 'priority'
  if (value === 'default' || value === 'standard') return 'standard'
  if (value === 'priority' || value === 'flex' || value === 'ultrafast') return value
  return value
}

export function formatUsageServiceTier(serviceTier?: string | null): string {
  const normalized = normalizeUsageServiceTier(serviceTier)
  if (!normalized) return 'standard'
  return normalized
}

export function getUsageServiceTierLabel(
  serviceTier: string | null | undefined,
  translate: (key: string) => string,
): string {
  const tier = formatUsageServiceTier(serviceTier)
  if (tier === 'priority') return translate('usage.serviceTierPriority')
  if (tier === 'ultrafast') return translate('usage.serviceTierUltrafast')
  if (tier === 'flex') return translate('usage.serviceTierFlex')
  if (tier === 'standard') return translate('usage.serviceTierStandard')
  return tier
}

export type UsageFastChannel = 'none' | 'ws' | 'http'

// getUsageFastChannel tells whether a billable Fast/Ultra Fast request actually
// used the upstream Responses WebSocket. The ChatGPT Codex backend only schedules
// priority/ultrafast over WS, so a priority row with openai_ws_mode=false is the
// useful "fast likely did not apply" signal; the response service_tier echo is not.
export function getUsageFastChannel(
  serviceTier: string | null | undefined,
  openAIWSMode: boolean | null | undefined,
): UsageFastChannel {
  const tier = normalizeUsageServiceTier(serviceTier)
  if (tier !== 'priority' && tier !== 'ultrafast') return 'none'
  return openAIWSMode ? 'ws' : 'http'
}

export function getUsageFastChannelLabel(
  channel: UsageFastChannel,
  translate: (key: string) => string,
): string {
  if (channel === 'ws') return translate('usage.fastChannelWS')
  if (channel === 'http') return translate('usage.fastChannelHTTP')
  return ''
}
