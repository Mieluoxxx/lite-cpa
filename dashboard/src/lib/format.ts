import type { RequestLog } from './api'

const WEEKDAYS = ['周日', '周一', '周二', '周三', '周四', '周五', '周六']

export function compact(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '0'
  // Force English locale so token counts always render as K/M/B,
  // regardless of the browser language (zh would show 万/亿 otherwise).
  return new Intl.NumberFormat('en', {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(value)
}

export function formatInteger(value: number): string {
  return new Intl.NumberFormat().format(value || 0)
}

export function formatRate(value: number | null): string {
  if (value === null || !Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(undefined, { style: 'percent', maximumFractionDigits: 1 }).format(value)
}

export function requestTime(value: string): { date: string; time: string } {
  const date = new Date(value)
  if (Number.isNaN(date.valueOf())) return { date: value || '—', time: '' }
  return {
    date: date.toLocaleDateString(undefined, { month: '2-digit', day: '2-digit' }),
    time: date.toLocaleTimeString(undefined, {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    }),
  }
}

export function formatDateTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.valueOf()) ? value || '—' : date.toLocaleString()
}

export function tokensPerSecond(record: RequestLog): string {
  if (!record.usage_complete || record.output_tokens <= 0 || record.duration_ms <= 0) return '—'
  return ((record.output_tokens * 1000) / record.duration_ms).toFixed(1)
}

export function outcomeLabel(outcome: string): string {
  if (outcome === 'completed') return '成功'
  if (outcome === 'error') return '错误'
  if (outcome === 'client_canceled') return '客户端取消'
  return '未知'
}

export function outcomeTone(outcome: string): string {
  if (outcome === 'completed') return 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300'
  if (outcome === 'error') return 'bg-red-500/12 text-red-700 dark:text-red-300'
  if (outcome === 'client_canceled') return 'bg-amber-500/12 text-amber-700 dark:text-amber-300'
  return 'bg-muted text-muted-foreground'
}

export function httpStatus(status: number): string {
  return status > 0 ? String(status) : '未提交'
}

export function displayedTokens(value: number, complete: boolean): string {
  if (complete) return compact(value)
  return value > 0 ? `${compact(value)}+` : '—'
}

export function detailedTokens(value: number, complete: boolean): string {
  if (complete) return formatInteger(value)
  return value > 0 ? `${formatInteger(value)}（至少）` : '未知'
}

export function dayKey(date: Date): string {
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${date.getFullYear()}-${month}-${day}`
}

export function parseDayKey(key: string): Date {
  const [year, month, day] = key.split('-').map(Number)
  return new Date(year, month - 1, day)
}

// Calendar arithmetic stays on local dates so DST shifts never skip or repeat a day.
export function addDays(date: Date, days: number): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate() + days)
}

export function startOfToday(): Date {
  return addDays(new Date(), 0)
}

export function formatDay(key: string, withWeekday = true): string {
  const date = parseDayKey(key)
  const base = `${date.getMonth() + 1}月${date.getDate()}日`
  return withWeekday ? `${base} ${WEEKDAYS[date.getDay()]}` : base
}

export function relativeTime(time: number, now = Date.now()): string {
  const seconds = Math.max(0, Math.round((now - time) / 1000))
  if (seconds < 60) return '刚刚'
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  return `${Math.floor(seconds / 86400)} 天前`
}
