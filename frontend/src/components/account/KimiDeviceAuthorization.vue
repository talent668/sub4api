<template>
  <div class="space-y-4 rounded-lg border border-blue-200 bg-blue-50 p-4 dark:border-blue-700 dark:bg-blue-900/30" data-testid="kimi-device-authorization">
    <div>
      <h4 class="font-semibold text-blue-900 dark:text-blue-200">{{ t('admin.accounts.kimiOAuth.title') }}</h4>
      <p class="mt-2 text-sm text-blue-700 dark:text-blue-300">{{ t('admin.accounts.kimiOAuth.description') }}</p>
    </div>
    <template v-if="grant">
      <p class="text-sm text-gray-700 dark:text-gray-200">{{ t('admin.accounts.kimiOAuth.userCode') }}</p>
      <p class="select-all rounded-lg bg-white px-4 py-3 text-center font-mono text-2xl tracking-widest text-gray-900 dark:bg-dark-800 dark:text-white">{{ grant.user_code }}</p>
      <a :href="grant.verification_uri" target="_blank" rel="noopener noreferrer" class="btn btn-primary inline-flex">{{ t('admin.accounts.kimiOAuth.openLink') }}</a>
    </template>
    <p v-if="waiting" role="status" class="text-sm text-blue-700 dark:text-blue-300">{{ t('admin.accounts.kimiOAuth.waiting') }}</p>
    <p v-if="credentials" role="status" class="text-sm text-green-700 dark:text-green-300">{{ t('admin.accounts.kimiOAuth.authorized') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    <button v-if="credentials" type="button" class="btn btn-primary" :disabled="busy" @click="emit('authorized', credentials)">{{ t('common.save') }}</button>
    <button v-else type="button" class="btn btn-secondary" :disabled="starting || busy || waiting" @click="start">{{ t(starting ? 'admin.accounts.kimiOAuth.starting' : 'admin.accounts.kimiOAuth.start') }}</button>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { kimiOAuthAPI, type KimiDeviceAuthorization } from '@/api/admin/kimi'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ proxyId?: number | null; busy?: boolean }>()
const emit = defineEmits<{ authorized: [credentials: Record<string, unknown>] }>()
const { t } = useI18n()
const grant = ref<KimiDeviceAuthorization | null>(null)
const credentials = ref<Record<string, unknown> | null>(null)
const starting = ref(false)
const waiting = ref(false)
const error = ref('')
let generation = 0
let expiresAt = 0
let timer: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | undefined

function reset() {
  generation++
  clearTimeout(timer)
  controller?.abort()
  controller = undefined
  grant.value = null
  credentials.value = null
  starting.value = false
  waiting.value = false
  error.value = ''
}

function schedulePoll(interval: number, current: number) {
  timer = setTimeout(() => void poll(current), Math.max(5, interval || 5) * 1000)
}

async function poll(current: number) {
  if (current !== generation || !grant.value) return
  if (Date.now() >= expiresAt) {
    waiting.value = false
    error.value = t('admin.accounts.kimiOAuth.expired')
    return
  }
  try {
    const result = await kimiOAuthAPI.poll(grant.value.session_id, controller?.signal)
    if (current !== generation) return
    if (result.status === 'authorized' && result.credentials) {
      waiting.value = false
      credentials.value = result.credentials
      emit('authorized', result.credentials)
      return
    }
    schedulePoll(result.interval, current)
  } catch (err) {
    if (current !== generation) return
    waiting.value = false
    error.value = extractApiErrorMessage(err, t('admin.accounts.kimiOAuth.failed'))
  }
}

async function start() {
  reset()
  const current = generation
  controller = new AbortController()
  starting.value = true
  try {
    const result = await kimiOAuthAPI.start(props.proxyId, controller.signal)
    if (current !== generation) return
    grant.value = result
    expiresAt = Date.now() + result.expires_in * 1000
    waiting.value = true
    schedulePoll(result.interval, current)
  } catch (err) {
    if (current !== generation) return
    error.value = extractApiErrorMessage(err, t('admin.accounts.kimiOAuth.failed'))
  } finally {
    if (current === generation) starting.value = false
  }
}

watch(() => props.proxyId, reset)
onBeforeUnmount(reset)
</script>
