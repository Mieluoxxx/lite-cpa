import { addDays, parseDayKey, startOfToday } from './format'

export type LogRange = 'today' | '7d' | '30d' | 'all'

export type LogFilters = {
  model: string
  upstream: string
  protocol: string
  status: string
  outcome: string
  range: LogRange
  // A drilled-down local day overrides range.
  day: string
}

export const EMPTY_FILTERS: LogFilters = { model: '', upstream: '', protocol: '', status: '', outcome: '', range: '7d', day: '' }

export const RANGE_OPTIONS: { value: LogRange; label: string }[] = [
  { value: 'today', label: '今天' },
  { value: '7d', label: '近 7 天' },
  { value: '30d', label: '近 30 天' },
  { value: 'all', label: '全部' },
]

const RANGE_DAYS: Record<Exclude<LogRange, 'all'>, number> = { today: 1, '7d': 7, '30d': 30 }

// Windows snap to local midnight so the query string stays stable between
// polls; an ever-moving "now - 7d" would defeat request de-duplication.
export function logWindow(filters: LogFilters): { from?: number; to?: number } {
  const seconds = (date: Date) => Math.floor(date.getTime() / 1000)
  if (filters.day) {
    const start = parseDayKey(filters.day)
    return { from: seconds(start), to: seconds(addDays(start, 1)) }
  }
  if (filters.range === 'all') return {}
  return { from: seconds(addDays(startOfToday(), 1 - RANGE_DAYS[filters.range])) }
}

export function logQuery(filters: LogFilters, userAgents: readonly string[] | null): URLSearchParams {
  const query = new URLSearchParams()
  const { from, to } = logWindow(filters)
  if (from !== undefined) query.set('from', String(from))
  if (to !== undefined) query.set('to', String(to))
  for (const ua of userAgents ?? []) query.append('ua', ua)
  return query
}

export function appendFieldFilters(query: URLSearchParams, filters: LogFilters): URLSearchParams {
  if (filters.model) query.set('model', filters.model)
  if (filters.upstream) query.set('upstream', filters.upstream)
  if (filters.protocol) query.set('protocol', filters.protocol)
  if (filters.status) query.set('status', filters.status)
  if (filters.outcome) query.set('outcome', filters.outcome)
  return query
}
