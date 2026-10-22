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
          <Toggle v-model="form.enabled" data-testid="openai-fast-bridge-toggle" />
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
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { useAppStore } from '@/stores'
import {
  getOpenAIFastBridgeSettings,
  updateOpenAIFastBridgeSettings,
} from '@/api/admin/settings'

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(true)
const saving = ref(false)
const form = reactive({ enabled: false })

async function load() {
  loading.value = true
  try {
    const settings = await getOpenAIFastBridgeSettings()
    form.enabled = settings.enabled
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
    const updated = await updateOpenAIFastBridgeSettings({ enabled: form.enabled })
    form.enabled = updated.enabled
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
