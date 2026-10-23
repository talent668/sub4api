<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { OpsRequestDiagnostics, OpsSystemLog } from '@/api/admin/ops'
import DiagnosticJsonBlock from './DiagnosticJsonBlock.vue'
import { diagnosticSnapshotSections, diagnosticUsageFields, projectDiagnosticSnapshots } from '../utils/diagnosticSnapshot'

const props = defineProps<{ row: OpsSystemLog; summary?: boolean }>()
const { t } = useI18n()
const durationFields = [
  'prep_ms', 'acquire_ms', 'prewrite_ms', 'write_ms',
  'upstream_first_event_wait_ms', 'upstream_first_semantic_wait_ms',
  'first_semantic_ms', 'first_text_ms', 'attempt_duration_ms', 'conn_pick_ms', 'queue_wait_ms'
] as const satisfies ReadonlyArray<keyof OpsRequestDiagnostics>
const summaryFields = ['first_semantic_ms', 'first_text_ms', 'attempt_duration_ms'] as const
const booleanFields = ['conn_reused', 'force_new_conn'] as const
const textFields = ['ingress', 'transport', 'outcome'] as const
const metadataFields = ['request_id', 'client_request_id', 'user_id', 'api_key_id', 'account_id', 'platform', 'model'] as const
const snapshots = computed(() => projectDiagnosticSnapshots(props.row.extra))
const unreportedUsage = computed(() => diagnosticUsageFields.filter(key => snapshots.value.usage?.[key] === undefined))
const missing = () => t('admin.ops.systemLogs.notRecorded')
function duration(key: typeof durationFields[number]) {
  const value: unknown = props.row.extra?.[key]
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? `${value} ms` : missing()
}
function booleanValue(key: typeof booleanFields[number]) {
  const value: unknown = props.row.extra?.[key]
  return typeof value === 'boolean' ? String(value) : missing()
}
function textValue(key: typeof textFields[number]) {
  const value: unknown = props.row.extra?.[key]
  return typeof value === 'string' && value.trim() ? value.slice(0, 120) : missing()
}
function metadata(key: typeof metadataFields[number]) {
  const value = props.row[key]
  if (typeof value === 'number' && Number.isFinite(value)) return String(value)
  return typeof value === 'string' && value ? value : missing()
}
</script>

<template>
  <div class="space-y-3" data-testid="request-diagnostic">
    <div class="break-all font-medium">openai.messages_ws_latency</div>
    <dl class="grid gap-x-6 gap-y-2 text-xs sm:grid-cols-2 lg:grid-cols-3">
      <div v-for="key in textFields" :key="key">
        <dt class="text-gray-500 dark:text-gray-400">{{ t(`admin.ops.systemLogs.phases.${key}`) }}</dt>
        <dd class="break-words text-gray-900 dark:text-gray-100">{{ textValue(key) }}</dd>
      </div>
      <div v-for="key in (summary ? summaryFields : durationFields)" :key="key">
        <dt class="text-gray-500 dark:text-gray-400">{{ t(`admin.ops.systemLogs.phases.${key}`) }}</dt>
        <dd class="tabular-nums text-gray-900 dark:text-gray-100">{{ duration(key) }}</dd>
      </div>
      <template v-if="!summary">
        <div v-for="key in booleanFields" :key="key">
          <dt class="text-gray-500 dark:text-gray-400">{{ t(`admin.ops.systemLogs.phases.${key}`) }}</dt>
          <dd class="text-gray-900 dark:text-gray-100">{{ booleanValue(key) }}</dd>
        </div>
      </template>
    </dl>
    <p v-if="!summary" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.phaseTimingHint') }}</p>
    <dl class="grid gap-2 border-t border-gray-200 pt-3 text-xs dark:border-dark-700 sm:grid-cols-2">
      <div v-for="key in metadataFields" :key="key">
        <dt class="text-gray-500 dark:text-gray-400">{{ key }}</dt>
        <dd class="break-all text-gray-900 dark:text-gray-100">{{ metadata(key) }}</dd>
      </div>
    </dl>
    <div v-if="!summary" class="space-y-3 border-t border-gray-200 pt-3 dark:border-dark-700" data-testid="diagnostic-snapshots">
      <h4 class="text-sm font-medium text-gray-900 dark:text-gray-100">{{ t('admin.ops.systemLogs.snapshots.title') }}</h4>
      <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.snapshots.safetyHint') }}</p>
      <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.snapshots.requestedActualHint') }}</p>
      <DiagnosticJsonBlock
        v-for="section in diagnosticSnapshotSections"
        :key="section"
        :section="section"
        :value="snapshots[section]"
        :title="t(`admin.ops.systemLogs.snapshots.sections.${section}`)"
        :empty-text="section === 'usage' ? t('admin.ops.systemLogs.snapshots.notReported') : undefined"
        :hint="section === 'upstream_request' ? t('admin.ops.systemLogs.snapshots.writeHint')
          : section === 'upstream_request_headers' ? t('admin.ops.systemLogs.snapshots.proposedHeadersHint')
            : section === 'upstream_handshake_headers' ? t('admin.ops.systemLogs.snapshots.handshakeHint')
              : section === 'usage' ? t('admin.ops.systemLogs.snapshots.usageHint') : undefined"
      >
        <dl v-if="section === 'usage' && snapshots.usage && unreportedUsage.length" class="grid gap-x-6 gap-y-1 text-xs sm:grid-cols-2" data-testid="diagnostic-unreported-usage">
          <div v-for="key in unreportedUsage" :key="key" class="flex flex-wrap justify-between gap-x-3">
            <dt class="text-gray-500 dark:text-gray-400">{{ t(`admin.ops.systemLogs.snapshots.usageFields.${key}`) }}</dt>
            <dd class="text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.snapshots.notReported') }}</dd>
          </div>
        </dl>
      </DiagnosticJsonBlock>
    </div>
  </div>
</template>
