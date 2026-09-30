export type RequestLog = {
  id: number
  request_id: string
  timestamp: string
  method: string
  path: string
  status_code: number
  outcome: string
  model: string
  protocol: string
  provider: string
  upstream: string
  user_agent: string
  duration_ms: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  usage_complete: boolean
  retried: boolean
  error: string
  req_body?: string
  resp_body?: string
}

export type LogPage = {
  items: RequestLog[]
  total: number
  limit: number
  offset: number
}

export type NameCount = {
  name: string
  count: number
}

export type LogStats = {
  enabled: boolean
  total: number
  by_model: NameCount[]
  by_upstream: NameCount[]
  by_status: NameCount[]
}

export type UsageMeta = {
  enabled: boolean
  store_body: boolean
  retention_seconds: number
  body_retention_seconds: number
}

export type UsageCounts = {
  user_agent: string
  attempts: number
  requests: number
  success: number
  errors: number
  canceled: number
  incomplete: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  last_seen_ms: number
}

export type DailyUsageRow = UsageCounts & { day: string; model: string }

export type WeekHourUsageRow = UsageCounts & { weekday: number; hour_of_day: number }

export type UsageResponse<T> = {
  meta: UsageMeta
  bucket: string
  first_day: string
  last_day: string
  items: T[]
}

export const API_KEY_STORAGE = 'lite-cpa-api-key'

export class APIError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export function savedAPIKey(): string {
  return localStorage.getItem(API_KEY_STORAGE) ?? ''
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const key = savedAPIKey()
  const response = await fetch(path, {
    ...init,
    headers: {
      ...init?.headers,
      ...(key ? { Authorization: `Bearer ${key}` } : {}),
    },
  })
  if (!response.ok) {
    throw new APIError(response.status, await response.text())
  }
  return response.json() as Promise<T>
}
