<template>
  <div class="card" data-testid="openai-fast-bridge-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
        {{ t("admin.settings.openaiFastBridge.title") }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t("admin.settings.openaiFastBridge.description") }}
      </p>
    </div>
    <div class="space-y-5 p-6">
      <div v-if="loading" class="flex items-center gap-2 text-gray-500">
        <div class="h-4 w-4 animate-spin rounded-full border-b-2 border-primary-600"></div>
        {{ t("common.loading") }}
      </div>

      <template v-else>
        <div class="flex items-center justify-between">
          <div>
            <label class="font-medium text-gray-900 dark:text-white">
              {{ t("admin.settings.openaiFastBridge.enabled") }}
            </label>
            <p class="text-sm text-gray-500 dark:text-gray-400">
              {{ t("admin.settings.openaiFastBridge.enabledHint") }}
            </p>
          </div>
          <Toggle v-model="form.enabled" data-testid="openai-fast-bridge-toggle" :disabled="saving" />
        </div>

        <div class="space-y-2">
          <label for="openai-fast-bridge-scope" class="font-medium text-gray-900 dark:text-white">
            {{ t("admin.settings.openaiFastBridge.scope") }}
          </label>
          <Select
            id="openai-fast-bridge-scope"
            v-model="form.scope"
            :options="scopeOptions"
            :disabled="!form.enabled || saving"
            :aria-label="t('admin.settings.openaiFastBridge.scope')"
            aria-describedby="openai-fast-bridge-scope-hint"
            data-testid="openai-fast-bridge-scope"
          />
          <p id="openai-fast-bridge-scope-hint" class="text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.openaiFastBridge.scopeHint") }}
          </p>
        </div>

        <p class="text-xs text-gray-500 dark:text-gray-400">
          {{ t("admin.settings.openaiFastBridge.note") }}
        </p>

        <div class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700">
          <button
            type="button"
            class="btn btn-primary btn-sm"
            data-testid="openai-fast-bridge-save"
            :disabled="saving"
            @click="save"
          >
            {{ saving ? t("common.saving") : t("common.save") }}
          </button>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import { useAppStore } from '@/stores'
import {
  getOpenAIFastBridgeSettings,
  updateOpenAIFastBridgeSettings,
  type OpenAIFastBridgeSettings,
} from '@/api/admin/settings'

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(true)
const saving = ref(false)
const form = reactive<OpenAIFastBridgeSettings>({ enabled: false, scope: 'all' })
const scopeOptions = computed(() => [
  { value: 'all', label: t('admin.settings.openaiFastBridge.scopeAll') },
  { value: 'fast_only', label: t('admin.settings.openaiFastBridge.scopeFastOnly') },
])

async function load() {
  loading.value = true
  try {
    const settings = await getOpenAIFastBridgeSettings()
    form.enabled = settings.enabled
    form.scope = settings.scope ?? 'all'
  } catch (error) {
    appStore.showError(
      (error as { message?: string })?.message ??
        t('admin.settings.openaiFastBridge.loadFailed'),
    )
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    const updated = await updateOpenAIFastBridgeSettings({
      enabled: form.enabled,
      scope: form.scope,
    })
    form.enabled = updated.enabled
    form.scope = updated.scope ?? 'all'
    appStore.showSuccess(t('admin.settings.openaiFastBridge.saved'))
  } catch (error) {
    appStore.showError(
      (error as { message?: string })?.message ??
        t('admin.settings.openaiFastBridge.saveFailed'),
    )
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>
