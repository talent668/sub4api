import apiClient from '../client'

export interface KimiDeviceAuthorization {
  session_id: string
  user_code: string
  verification_uri: string
  expires_in: number
  interval: number
}

export interface KimiDevicePoll {
  status: 'pending' | 'authorized'
  interval: number
  credentials?: Record<string, unknown>
}

export const kimiOAuthAPI = {
  async start(proxyId?: number | null, signal?: AbortSignal): Promise<KimiDeviceAuthorization> {
    const { data } = await apiClient.post('/admin/kimi/oauth/device', { proxy_id: proxyId }, { signal })
    return data
  },
  async poll(sessionId: string, signal?: AbortSignal): Promise<KimiDevicePoll> {
    const { data } = await apiClient.post('/admin/kimi/oauth/poll', { session_id: sessionId }, { signal })
    return data
  }
}
