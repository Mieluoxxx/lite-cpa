import { agentFromUA, type Agent } from './agents'
import type { DailyUsageRow, RequestLog, UsageCounts, WeekHourUsageRow } from './api'
import { compact, formatInteger } from './format'

export type LogRecord = RequestLog & { agent: Agent }

export function enrichLog(item: RequestLog): LogRecord {
  return { ...item, agent: agentFromUA(item.user_agent) }
}

export type UsageEntry = DailyUsageRow & { agent: Agent }

export function enrichUsage(rows: readonly DailyUsageRow[]): UsageEntry[] {
  return rows.map((row) => ({ ...row, agent: agentFromUA(row.user_agent) }))
}

export type WeekHourEntry = WeekHourUsageRow & { agent: Agent }

export function enrichWeekHour(rows: readonly WeekHourUsageRow[]): WeekHourEntry[] {
  return rows.map((row) => ({ ...row, agent: agentFromUA(row.user_agent) }))
}

// Requests, outcomes and incomplete counts come from each request's final row;
// attempts also count retried failures. Token sums only include complete usage.
export type Totals = {
  attempts: number
  requests: number
  success: number
  errors: number
  canceled: number
  incomplete: number
  input: number
  output: number
  cached: number
  lastSeen: number
}

export function emptyTotals(): Totals {
  return { attempts: 0, requests: 0, success: 0, errors: 0, canceled: 0, incomplete: 0, input: 0, output: 0, cached: 0, lastSeen: 0 }
}

export function addToTotals(totals: Totals, row: UsageCounts): void {
  totals.attempts += row.attempts
  totals.requests += row.requests
  totals.success += row.success
  totals.errors += row.errors
  totals.canceled += row.canceled
  totals.incomplete += row.incomplete
  totals.input += row.input_tokens
  totals.output += row.output_tokens
  totals.cached += row.cached_tokens
  if (row.last_seen_ms > totals.lastSeen) totals.lastSeen = row.last_seen_ms
}

export function summarize(rows: readonly UsageCounts[]): Totals {
  const totals = emptyTotals()
  for (const row of rows) addToTotals(totals, row)
  return totals
}

export function totalTokens(totals: Totals): number {
  return totals.input + totals.output
}

export function cacheHitRate(totals: Totals): number | null {
  return totals.input > 0 ? totals.cached / totals.input : null
}

export function successRate(totals: Totals): number | null {
  const resolved = totals.success + totals.errors
  return resolved > 0 ? totals.success / resolved : null
}

export type Metric = 'tokens' | 'output' | 'requests'

export const METRIC_OPTIONS: { value: Metric; label: string }[] = [
  { value: 'tokens', label: '总 Token' },
  { value: 'output', label: '输出' },
  { value: 'requests', label: '请求' },
]

export function metricValue(totals: Totals, metric: Metric): number {
  if (metric === 'requests') return totals.requests
  if (metric === 'output') return totals.output
  return totalTokens(totals)
}

export function formatMetric(value: number, metric: Metric): string {
  if (metric === 'requests') return `${formatInteger(value)} 次请求`
  return `${compact(value)} ${metric === 'output' ? '输出 tokens' : 'tokens'}`
}

export function compareUsage(a: { totals: Totals }, b: { totals: Totals }): number {
  return totalTokens(b.totals) - totalTokens(a.totals) || b.totals.requests - a.totals.requests
}

export type ModelUsage = {
  model: string
  totals: Totals
}

export type AgentUsage = {
  agent: Agent
  totals: Totals
  models: ModelUsage[]
  userAgents: string[]
}

export function usageByAgent(entries: readonly UsageEntry[]): AgentUsage[] {
  const groups = new Map<string, { agent: Agent; totals: Totals; models: Map<string, Totals>; userAgents: Set<string> }>()
  for (const entry of entries) {
    let group = groups.get(entry.agent.id)
    if (!group) {
      group = { agent: entry.agent, totals: emptyTotals(), models: new Map(), userAgents: new Set() }
      groups.set(entry.agent.id, group)
    }
    addToTotals(group.totals, entry)
    let model = group.models.get(entry.model)
    if (!model) {
      model = emptyTotals()
      group.models.set(entry.model, model)
    }
    addToTotals(model, entry)
    group.userAgents.add(entry.user_agent)
  }
  return [...groups.values()]
    .map((group) => ({
      agent: group.agent,
      totals: group.totals,
      models: [...group.models].map(([model, totals]) => ({ model, totals })).sort(compareUsage),
      userAgents: [...group.userAgents],
    }))
    .sort(compareUsage)
}

export type DayUsage = {
  totals: Totals
  agents: Map<string, { agent: Agent; totals: Totals }>
}

export function usageByDay(entries: readonly UsageEntry[]): Map<string, DayUsage> {
  const days = new Map<string, DayUsage>()
  for (const entry of entries) {
    let day = days.get(entry.day)
    if (!day) {
      day = { totals: emptyTotals(), agents: new Map() }
      days.set(entry.day, day)
    }
    addToTotals(day.totals, entry)
    let agent = day.agents.get(entry.agent.id)
    if (!agent) {
      agent = { agent: entry.agent, totals: emptyTotals() }
      day.agents.set(entry.agent.id, agent)
    }
    addToTotals(agent.totals, entry)
  }
  return days
}

// Index is weekday * 24 + hour with Monday as weekday 0.
export function weekHourCells(entries: readonly WeekHourEntry[]): Totals[] {
  const cells = Array.from({ length: 7 * 24 }, emptyTotals)
  for (const entry of entries) addToTotals(cells[entry.weekday * 24 + entry.hour_of_day], entry)
  return cells
}

// Rank-based levels keep one burst day from washing every other day into the
// lightest step, which a linear max-scaled ramp would do.
export function levelScale(values: readonly number[]): (value: number) => number {
  const sorted = values.filter((value) => value > 0).sort((a, b) => a - b)
  return (value) => {
    if (value <= 0 || sorted.length === 0) return 0
    return Math.max(1, Math.ceil((4 * upperBound(sorted, value)) / sorted.length))
  }
}

function upperBound(sorted: readonly number[], value: number): number {
  let low = 0
  let high = sorted.length
  while (low < high) {
    const mid = (low + high) >> 1
    if (sorted[mid] <= value) low = mid + 1
    else high = mid
  }
  return low
}
