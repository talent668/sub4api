<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useClipboard } from '@/composables/useClipboard'
import {
  projectDiagnosticSnapshot,
  type DiagnosticSnapshotSection
} from '../utils/diagnosticSnapshot'

const props = defineProps<{
  section: DiagnosticSnapshotSection
  value?: unknown
  title: string
  hint?: string
  emptyText?: string
}>()
const { t } = useI18n()
const { copyToClipboard } = useClipboard()
const safeValue = computed(() => projectDiagnosticSnapshot(props.section, props.value))
const safeJSON = computed(() => Object.keys(safeValue.value).length ? JSON.stringify(safeValue.value, null, 2) : '')
const tokens = computed(() => {
  // Input is ONLY the freshly projected object serialized above. Vue text
  // interpolation escapes each token; neither HTML nor arbitrary extra is used.
  const parts = safeJSON.value.match(/"(?:\\.|[^"\\])*"\s*(?=:)|"(?:\\.|[^"\\])*"|-?\d+(?:\.\d+)?(?:e[+-]?\d+)?|true|false|null|[{}\[\],:]|\s+/gi) ?? []
  return parts.map((text, index) => {
    let kind = 'punctuation'
    if (/^"/.test(text)) kind = parts[index + 1] === ':' ? 'key' : 'string'
    else if (/^(true|false)$/.test(text)) kind = 'boolean'
    else if (text === 'null') kind = 'null'
    else if (/^-?\d/.test(text)) kind = 'number'
    else if (/^\s+$/.test(text)) kind = 'whitespace'
    return { text, kind }
  })
})
async function copySafeJSON() {
  if (safeJSON.value) await copyToClipboard(safeJSON.value, t('admin.ops.systemLogs.snapshots.copied'))
}
</script>

<template>
  <details class="diagnostic-json-block rounded-lg border border-gray-200 dark:border-dark-700" :data-testid="`diagnostic-section-${section}`">
    <summary class="cursor-pointer rounded-lg px-3 py-2 text-xs font-medium text-gray-800 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 dark:text-gray-100">
      {{ title }}
    </summary>
    <div class="space-y-2 border-t border-gray-200 p-3 dark:border-dark-700">
      <p v-if="hint" class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ hint }}</p>
      <template v-if="safeJSON">
        <div class="flex justify-end">
          <button type="button" class="btn btn-secondary btn-sm text-xs" :data-testid="`copy-diagnostic-${section}`" :aria-label="`${t('admin.ops.systemLogs.snapshots.copy')} · ${title}`" @click="copySafeJSON">
            {{ t('admin.ops.systemLogs.snapshots.copy') }}
          </button>
        </div>
        <pre class="diagnostic-json max-h-80 overflow-auto whitespace-pre rounded-md bg-gray-50 p-3 font-mono text-xs leading-relaxed dark:bg-dark-900" :data-testid="`diagnostic-json-${section}`"><code><span v-for="(token, index) in tokens" :key="index" :class="`diagnostic-json-${token.kind}`">{{ token.text }}</span></code></pre>
      </template>
      <p v-else class="text-xs text-gray-500 dark:text-gray-400" data-testid="diagnostic-snapshot-missing">{{ emptyText || t('admin.ops.systemLogs.notRecorded') }}</p>
      <slot />
    </div>
  </details>
</template>

<style scoped>
.diagnostic-json-key { @apply text-indigo-700 dark:text-indigo-300; }
.diagnostic-json-string { @apply text-emerald-700 dark:text-emerald-300; }
.diagnostic-json-number { @apply text-blue-700 dark:text-blue-300; }
.diagnostic-json-boolean { @apply text-purple-700 dark:text-purple-300; }
.diagnostic-json-null { @apply text-gray-500 dark:text-gray-400; }
.diagnostic-json-punctuation,
.diagnostic-json-whitespace { @apply text-gray-600 dark:text-gray-300; }
</style>
