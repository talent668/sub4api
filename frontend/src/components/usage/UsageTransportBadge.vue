<template>
  <span data-testid="transport-chain" class="inline-flex flex-wrap items-center gap-1 rounded bg-gray-100 px-2 py-0.5 text-xs text-gray-600 dark:bg-dark-700 dark:text-gray-300" :title="description">
    {{ label }}
    <span v-if="verifiedFailure" data-testid="client-ws-failure" class="border-l border-gray-300 pl-1 dark:border-dark-500">{{ t('usage.clientWSFailureLinked') }}</span>
  </span>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UsageTransportDetails } from '@/types'
const props = defineProps<{ details?: UsageTransportDetails | null }>()
const { t } = useI18n()
const observed = computed(() => props.details && ['http', 'ws'].includes(props.details.client) && ['http', 'ws'].includes(props.details.upstream) ? props.details : null)
const label = computed(() => observed.value ? `${observed.value.client.toUpperCase()}→${observed.value.upstream.toUpperCase()}` : t('usage.transportNotRecorded'))
const verifiedFailure = computed(() => {
  const failure = observed.value?.client_ws_failure
  return observed.value?.client === 'http' && failure?.evidence === 'receipt_v1' && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(failure.attempt_id) ? failure : null
})
const description = computed(() => {
  if (!observed.value) return t('usage.transportNotRecordedHint')
  const parts = [label.value]
  const mode = observed.value.mode
  if (mode && ['ctx_pool', 'passthrough', 'http_bridge', 'http_ws_bridge'].includes(mode)) parts.push(t(`usage.transportModes.${mode}`))
  const reason = observed.value.fallback_reason
  if (reason && ['large_payload', 'plugin_http_bridge', 'provider_http_bridge', 'ws_failure'].includes(reason)) parts.push(t(`usage.transportFallbackReasons.${reason}`))
  if (verifiedFailure.value) parts.push(t('usage.clientWSFailureLinkedHint', { attemptId: verifiedFailure.value.attempt_id }))
  return parts.join(' · ')
})
</script>
