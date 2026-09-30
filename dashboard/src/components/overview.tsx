import { ArrowRight } from 'lucide-react'
import type React from 'react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { compareAgents, type Agent } from '../lib/agents'
import type { UsageMeta, UsageResponse, WeekHourUsageRow } from '../lib/api'
import { addDays, compact, dayKey, formatDay, formatInteger, formatRate, parseDayKey } from '../lib/format'
import { tooltipAt, type TooltipState } from '../lib/tooltip'
import {
  addToTotals,
  cacheHitRate,
  compareUsage,
  emptyTotals,
  enrichWeekHour,
  formatMetric,
  levelScale,
  METRIC_OPTIONS,
  metricValue,
  successRate,
  summarize,
  totalTokens,
  usageByDay,
  weekHourCells,
  type DayUsage,
  type Metric,
  type Totals,
  type UsageEntry,
} from '../lib/usage'
import { usePolling } from '../lib/use-polling'
import { AgentDot, ChartTooltip, MetricCard, Segmented, TokenMixBar, TokenMixLegend } from './chart-kit'

const WEEKS = 53
const WEEKDAY_SHORT = ['一', '二', '三', '四', '五', '六', '日']
const DAY_PANEL_ROWS = 8

type View = 'calendar' | 'hours' | 'table'

const VIEW_OPTIONS: { value: View; label: string }[] = [
  { value: 'calendar', label: '日历' },
  { value: 'hours', label: '时段' },
  { value: 'table', label: '表格' },
]

type SetTooltip = (state: TooltipState) => void

export type UsageScope = {
  days: number
  tz: number
  agentId: string | null
  version: unknown
}

export function OverviewView({ entries, meta, scope, selectedDay, onSelectDay, onDrillDay }: {
  entries: UsageEntry[]
  meta: UsageMeta | null
  scope: UsageScope
  selectedDay: string
  onSelectDay: (day: string) => void
  onDrillDay: (day: string) => void
}) {
  const totals = useMemo(() => summarize(entries), [entries])
  const byDay = useMemo(() => usageByDay(entries), [entries])
  const resolved = totals.success + totals.errors
  const retries = totals.attempts - totals.requests

  return (
    <div className="space-y-6">
      <section className="grid grid-cols-2 gap-3 lg:grid-cols-6" aria-label="近一年用量统计">
        <MetricCard label="总请求" value={formatInteger(totals.requests)} hint={retries > 0 ? `另有 ${formatInteger(retries)} 次失败尝试被重试` : '近一年，每个请求只计一次'} />
        <MetricCard label="输入 Token" value={compact(totals.input)} hint={totals.incomplete > 0 ? `${formatInteger(totals.input)} · ${formatInteger(totals.incomplete)} 条未完整` : formatInteger(totals.input)} />
        <MetricCard label="输出 Token" value={compact(totals.output)} hint={totals.incomplete > 0 ? `${formatInteger(totals.output)} · ${formatInteger(totals.incomplete)} 条未完整` : formatInteger(totals.output)} />
        <MetricCard label="成功率" value={formatRate(successRate(totals))} hint={`${formatInteger(totals.success)} / ${formatInteger(resolved)} 最终成功`} tone="text-emerald-700 dark:text-emerald-300" />
        <MetricCard label="客户端取消" value={formatInteger(totals.canceled)} hint="不计成功或上游错误" tone="text-amber-700 dark:text-amber-300" />
        <MetricCard label="缓存命中率" value={formatRate(cacheHitRate(totals))} hint={`${formatInteger(totals.cached)} / ${formatInteger(totals.input)} 缓存 Token`} tone="text-sky-700 dark:text-sky-300" />
      </section>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_22rem]">
        <HeatmapCard byDay={byDay} scope={scope} selectedDay={selectedDay} onSelectDay={onSelectDay} />
        <DayPanel day={selectedDay} entries={entries} meta={meta} onDrill={() => onDrillDay(selectedDay)} />
      </div>
    </div>
  )
}

function HeatmapCard({ byDay, scope, selectedDay, onSelectDay }: {
  byDay: Map<string, DayUsage>
  scope: UsageScope
  selectedDay: string
  onSelectDay: (day: string) => void
}) {
  const [view, setView] = useState<View>('calendar')
  const [metric, setMetric] = useState<Metric>('tokens')
  const [tooltip, setTooltip] = useState<TooltipState>(null)

  const summary = useMemo(() => {
    let active = 0
    let peak: { day: string; value: number } | null = null
    let earliest = ''
    for (const [day, usage] of byDay) {
      const value = metricValue(usage.totals, metric)
      if (value > 0) {
        active += 1
        if (!peak || value > peak.value) peak = { day, value }
      }
      if (!earliest || day < earliest) earliest = day
    }
    return { active, peak, earliest }
  }, [byDay, metric])

  function changeView(next: View) {
    setTooltip(null)
    setView(next)
  }

  return (
    <Card className="glass-surface min-w-0 rounded-[1.5rem]">
      <CardContent className="p-5 sm:p-6">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <h2 className="text-base font-semibold tracking-tight">每日用量</h2>
            <p className="mt-1 text-sm text-muted-foreground">
              {summary.active} 个活跃日
              {summary.peak && ` · 峰值 ${formatDay(summary.peak.day, false)} ${formatMetric(summary.peak.value, metric)}`}
            </p>
          </div>
          <div className="flex flex-wrap gap-2">
            <Segmented label="视图" value={view} options={VIEW_OPTIONS} onChange={changeView} />
            <Segmented label="指标" value={metric} options={METRIC_OPTIONS} onChange={setMetric} />
          </div>
        </div>

        <div className="mt-5">
          {view === 'calendar' && <CalendarGrid byDay={byDay} metric={metric} selectedDay={selectedDay} onSelectDay={onSelectDay} onTooltip={setTooltip} />}
          {view === 'hours' && <WeekHourGrid scope={scope} metric={metric} onTooltip={setTooltip} />}
          {view === 'table' && <DayTable byDay={byDay} selectedDay={selectedDay} onSelectDay={onSelectDay} />}
        </div>

        <div className="mt-4 flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground">
          <span>来自按小时的用量汇总，不受原始日志保留期影响{summary.earliest && `（最早 ${formatDay(summary.earliest, false)}）`}；token 只计 usage 完整的请求</span>
          {view !== 'table' && <HeatLegend />}
        </div>
      </CardContent>
      <ChartTooltip state={tooltip} />
    </Card>
  )
}

function buildCalendar(today: Date) {
  const start = addDays(today, -((today.getDay() + 6) % 7) - 7 * (WEEKS - 1))
  const weeks: (Date | null)[][] = []
  const months: { week: number; label: string }[] = []
  for (let w = 0; w < WEEKS; w++) {
    const week: (Date | null)[] = []
    for (let d = 0; d < 7; d++) {
      const date = addDays(start, w * 7 + d)
      week.push(date > today ? null : date)
    }
    const month = addDays(start, w * 7).getMonth()
    if (w === 0 || month !== addDays(start, (w - 1) * 7).getMonth()) months.push({ week: w, label: `${month + 1}月` })
    weeks.push(week)
  }
  if (months.length > 1 && months[1].week - months[0].week < 3) months.shift()
  return { start, weeks, months }
}

function CalendarGrid({ byDay, metric, selectedDay, onSelectDay, onTooltip }: {
  byDay: Map<string, DayUsage>
  metric: Metric
  selectedDay: string
  onSelectDay: (day: string) => void
  onTooltip: SetTooltip
}) {
  const todayKey = dayKey(new Date())
  const today = parseDayKey(todayKey)
  const calendar = useMemo(() => buildCalendar(parseDayKey(todayKey)), [todayKey])
  const level = useMemo(() => levelScale([...byDay.values()].map((usage) => metricValue(usage.totals, metric))), [byDay, metric])
  const gridRef = useRef<HTMLDivElement>(null)
  const focusPending = useRef(false)
  const startKey = dayKey(calendar.start)
  const focusable = selectedDay >= startKey && selectedDay <= todayKey ? selectedDay : todayKey

  // Move focus after the new day renders; rAF is throttled in background tabs.
  useEffect(() => {
    if (!focusPending.current) return
    focusPending.current = false
    gridRef.current?.querySelector<HTMLButtonElement>(`[data-day="${focusable}"]`)?.focus()
  }, [focusable])

  function show(element: Element, day: string) {
    const totals = byDay.get(day)?.totals
    const value = totals ? metricValue(totals, metric) : 0
    const detail = metric === 'requests' ? `${compact(totals ? totalTokens(totals) : 0)} tokens` : `${formatInteger(totals?.requests ?? 0)} 次请求`
    onTooltip(tooltipAt(element, <>
      <div className="font-semibold text-foreground">{formatMetric(value, metric)}</div>
      <div className="mt-0.5 text-muted-foreground">{formatDay(day)} · {detail}</div>
    </>))
  }

  function onKeyDown(event: React.KeyboardEvent) {
    const steps: Record<string, number> = { ArrowLeft: -7, ArrowRight: 7, ArrowUp: -1, ArrowDown: 1 }
    let next: Date
    if (event.key in steps) next = addDays(parseDayKey(focusable), steps[event.key])
    else if (event.key === 'Home') next = calendar.start
    else if (event.key === 'End') next = today
    else if (event.key === 'Escape') {
      onTooltip(null)
      return
    } else return
    event.preventDefault()
    const key = dayKey(next < calendar.start ? calendar.start : next > today ? today : next)
    focusPending.current = true
    onSelectDay(key)
  }

  return (
    <div className="overflow-x-auto pb-1">
      <div
        ref={gridRef}
        role="group"
        aria-label="每日用量日历，方向键切换日期"
        className="grid min-w-[42rem]"
        style={{ gridTemplateColumns: `2rem repeat(${WEEKS}, minmax(0, 1fr))` }}
        onKeyDown={onKeyDown}
      >
        {calendar.months.map((month) => (
          <span key={month.week} className="pb-1 text-[0.68rem] whitespace-nowrap text-muted-foreground" style={{ gridColumn: `${month.week + 2} / span 4`, gridRow: 1 }}>
            {month.label}
          </span>
        ))}
        {[0, 2, 4].map((d) => (
          <span key={d} className="self-center text-[0.68rem] text-muted-foreground" style={{ gridColumn: 1, gridRow: d + 2 }}>
            周{WEEKDAY_SHORT[d]}
          </span>
        ))}
        {calendar.weeks.map((week, w) => week.map((date, d) => {
          if (!date) return null
          const key = dayKey(date)
          const totals = byDay.get(key)?.totals
          const value = totals ? metricValue(totals, metric) : 0
          return (
            <button
              key={key}
              type="button"
              data-day={key}
              tabIndex={key === focusable ? 0 : -1}
              aria-label={`${formatDay(key)}：${formatMetric(value, metric)}`}
              aria-pressed={key === selectedDay}
              className="group/cell aspect-square cursor-pointer p-[1.5px] outline-none"
              style={{ gridColumn: w + 2, gridRow: d + 2 }}
              onClick={() => onSelectDay(key)}
              onMouseEnter={(event) => show(event.currentTarget, key)}
              onMouseLeave={() => onTooltip(null)}
              onFocus={(event) => show(event.currentTarget, key)}
              onBlur={() => onTooltip(null)}
            >
              <HeatSwatch level={level(value)} selected={key === selectedDay} />
            </button>
          )
        }))}
      </div>
    </div>
  )
}

function WeekHourGrid({ scope, metric, onTooltip }: { scope: UsageScope; metric: Metric; onTooltip: SetTooltip }) {
  const polled = usePolling<UsageResponse<WeekHourUsageRow>>(`/api/usage?bucket=weekhour&days=${scope.days}&tz=${scope.tz}`, 300_000, scope.version)
  const cells = useMemo(() => {
    const entries = enrichWeekHour(polled.data?.items ?? [])
    return weekHourCells(scope.agentId ? entries.filter((entry) => entry.agent.id === scope.agentId) : entries)
  }, [polled.data, scope.agentId])
  const values = useMemo(() => cells.map((totals) => metricValue(totals, metric)), [cells, metric])
  const level = useMemo(() => levelScale(values), [values])
  const pad = (hour: number) => String(hour).padStart(2, '0')

  return (
    <div className={cn('overflow-x-auto pb-1 transition-opacity', polled.loading && 'opacity-60')}>
      <div role="group" aria-label="按星期和小时的用量分布" className="grid min-w-[36rem]" style={{ gridTemplateColumns: '2rem repeat(24, minmax(0, 1fr))' }}>
        {[0, 6, 12, 18].map((hour) => (
          <span key={hour} className="pb-1 text-[0.68rem] text-muted-foreground" style={{ gridColumn: `${hour + 2} / span 3`, gridRow: 1 }}>
            {hour}时
          </span>
        ))}
        {WEEKDAY_SHORT.map((label, d) => (
          <span key={label} className="self-center text-[0.68rem] text-muted-foreground" style={{ gridColumn: 1, gridRow: d + 2 }}>
            周{label}
          </span>
        ))}
        {values.map((value, index) => {
          const d = Math.floor(index / 24)
          const hour = index % 24
          const label = `周${WEEKDAY_SHORT[d]} ${pad(hour)}:00–${pad(hour + 1)}:00`
          return (
            <span
              key={index}
              role="img"
              aria-label={`${label}：${formatMetric(value, metric)}`}
              className="group/cell h-6 p-[1.5px]"
              style={{ gridColumn: hour + 2, gridRow: d + 2 }}
              onMouseEnter={(event) => onTooltip(tooltipAt(event.currentTarget, <>
                <div className="font-semibold text-foreground">{formatMetric(value, metric)}</div>
                <div className="mt-0.5 text-muted-foreground">{label}</div>
              </>))}
              onMouseLeave={() => onTooltip(null)}
            >
              <HeatSwatch level={level(value)} />
            </span>
          )
        })}
      </div>
    </div>
  )
}

function DayTable({ byDay, selectedDay, onSelectDay }: { byDay: Map<string, DayUsage>; selectedDay: string; onSelectDay: (day: string) => void }) {
  const rows = useMemo(() => [...byDay].sort((a, b) => b[0].localeCompare(a[0])), [byDay])
  if (rows.length === 0) return <p className="py-12 text-center text-sm text-muted-foreground">暂无请求记录。</p>

  return (
    <div className="glass-divider max-h-80 overflow-auto rounded-xl border">
      <table className="w-full min-w-[40rem] text-left text-sm">
        <thead className="glass-divider sticky top-0 border-b bg-card/95 text-xs text-muted-foreground backdrop-blur">
          <tr>
            <th className="px-3 py-2 font-medium">日期</th>
            <th className="px-3 py-2 text-right font-medium">请求</th>
            <th className="px-3 py-2 text-right font-medium">缓存输入</th>
            <th className="px-3 py-2 text-right font-medium">未缓存输入</th>
            <th className="px-3 py-2 text-right font-medium">输出</th>
            <th className="px-3 py-2 font-medium">Agent</th>
          </tr>
        </thead>
        <tbody className="glass-divider divide-y">
          {rows.map(([day, usage]) => {
            const cached = Math.min(usage.totals.cached, usage.totals.input)
            const agents = [...usage.agents.values()].sort((a, b) => compareAgents(a.agent, b.agent))
            return (
              <tr key={day} className={cn('glass-row cursor-pointer', day === selectedDay && 'bg-accent')} onClick={() => onSelectDay(day)}>
                <td className="px-3 py-2 whitespace-nowrap">{formatDay(day)}</td>
                <td className="px-3 py-2 text-right tabular-nums">{formatInteger(usage.totals.requests)}</td>
                <td className="px-3 py-2 text-right tabular-nums">{compact(cached)}</td>
                <td className="px-3 py-2 text-right tabular-nums">{compact(usage.totals.input - cached)}</td>
                <td className="px-3 py-2 text-right tabular-nums">{compact(usage.totals.output)}</td>
                <td className="px-3 py-2">
                  <div className="flex flex-wrap gap-x-3 gap-y-1">
                    {agents.map(({ agent }) => (
                      <span key={agent.id} className="inline-flex items-center gap-1.5 text-xs"><AgentDot agent={agent} className="size-2" />{agent.name}</span>
                    ))}
                  </div>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function HeatSwatch({ level, selected = false }: { level: number; selected?: boolean }) {
  return (
    <span
      className={cn(
        'block size-full rounded-[3px] transition-[filter] group-hover/cell:brightness-110 group-focus-visible/cell:outline-2 group-focus-visible/cell:outline-offset-1 group-focus-visible/cell:outline-ring',
        selected && 'outline-2 outline-offset-1 outline-foreground/80',
      )}
      style={{ background: `var(--heat-${level})` }}
    />
  )
}

function HeatLegend() {
  return (
    <div className="flex items-center gap-1.5">
      <span>少</span>
      {[0, 1, 2, 3, 4].map((level) => (
        <span key={level} aria-hidden className="size-3 rounded-[3px]" style={{ background: `var(--heat-${level})` }} />
      ))}
      <span>多</span>
    </div>
  )
}

// Raw rows older than the row retention are gone; only the rollup remains.
function rawLogsAvailable(day: string, meta: UsageMeta | null): boolean {
  if (!meta || meta.retention_seconds <= 0) return true
  return addDays(parseDayKey(day), 1).getTime() > Date.now() - meta.retention_seconds * 1000
}

function DayPanel({ day, entries, meta, onDrill }: { day: string; entries: UsageEntry[]; meta: UsageMeta | null; onDrill: () => void }) {
  const dayEntries = useMemo(() => entries.filter((entry) => entry.day === day), [entries, day])
  const totals = useMemo(() => summarize(dayEntries), [dayEntries])
  const rows = useMemo(() => {
    const groups = new Map<string, { agent: Agent; model: string; totals: Totals }>()
    for (const entry of dayEntries) {
      const key = `${entry.agent.id}\u0000${entry.model}`
      let group = groups.get(key)
      if (!group) {
        group = { agent: entry.agent, model: entry.model, totals: emptyTotals() }
        groups.set(key, group)
      }
      addToTotals(group.totals, entry)
    }
    return [...groups.values()].sort(compareUsage)
  }, [dayEntries])
  const isToday = day === dayKey(new Date())
  const drillable = rawLogsAvailable(day, meta)

  return (
    <Card className="glass-surface min-w-0 rounded-[1.5rem]">
      <CardContent className="flex h-full flex-col p-5 sm:p-6">
        <p className="text-[0.7rem] font-medium tracking-[0.08em] text-muted-foreground">当日明细</p>
        <h2 className="mt-1 flex items-center gap-2 text-base font-semibold tracking-tight">
          {formatDay(day)}
          {isToday && <span className="rounded-full bg-muted px-2 py-0.5 text-[0.68rem] font-medium text-muted-foreground">今天</span>}
        </h2>

        {totals.requests === 0 && totals.attempts === 0 ? (
          <p className="mt-6 text-sm text-muted-foreground">这一天没有请求记录。点击热力图中的其他日期查看。</p>
        ) : (
          <>
            <div className="mt-4">
              <p className="text-3xl font-semibold tracking-tight">{compact(totalTokens(totals))}</p>
              <p className="mt-1 text-xs text-muted-foreground">
                tokens（输入 + 输出）· {formatInteger(totals.requests)} 次请求 · 成功率 {formatRate(successRate(totals))}
              </p>
            </div>
            <div className="mt-5 space-y-3">
              <TokenMixBar totals={totals} />
              <TokenMixLegend totals={totals} />
            </div>
            <h3 className="mt-6 text-xs font-medium text-muted-foreground">Agent · 模型</h3>
            <ul className="glass-divider mt-1 divide-y">
              {rows.slice(0, DAY_PANEL_ROWS).map((row) => (
                <DayRow key={`${row.agent.id}/${row.model}`} row={row} />
              ))}
            </ul>
            {rows.length > DAY_PANEL_ROWS && <p className="mt-1 text-xs text-muted-foreground">另有 {rows.length - DAY_PANEL_ROWS} 项</p>}
            <div className="mt-auto pt-5">
              <Button variant="outline" size="sm" disabled={!drillable} onClick={onDrill}>查看当天请求 <ArrowRight /></Button>
              {!drillable && <p className="mt-2 text-xs text-muted-foreground">这一天的原始日志已超过保留期，只剩用量汇总。</p>}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  )
}

function DayRow({ row }: { row: { agent: Agent; model: string; totals: Totals } }) {
  return (
    <li className="flex items-center gap-2 py-2 text-sm">
      <AgentDot agent={row.agent} />
      <span className="min-w-0 flex-1 truncate" title={`${row.agent.name} · ${row.model || '(empty)'}`}>
        <span className="font-medium">{row.agent.name}</span>
        <span className="text-muted-foreground"> · </span>
        <span className="font-mono text-xs">{row.model || '(empty)'}</span>
      </span>
      <span className="shrink-0 text-right tabular-nums">
        <span className="font-medium">{compact(totalTokens(row.totals))}</span>
        <span className="ml-1.5 text-xs text-muted-foreground">{formatInteger(row.totals.requests)} 次</span>
      </span>
    </li>
  )
}
