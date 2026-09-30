import { ArrowRight, ChevronRight } from 'lucide-react'
import { Fragment, useMemo, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { compareAgents, type Agent } from '../lib/agents'
import { addDays, compact, dayKey, formatDay, formatInteger, formatRate, parseDayKey, relativeTime } from '../lib/format'
import {
  cacheHitRate,
  formatMetric,
  METRIC_OPTIONS,
  metricValue,
  successRate,
  totalTokens,
  usageByAgent,
  usageByDay,
  type AgentUsage,
  type Metric,
  type Totals,
  type UsageEntry,
} from '../lib/usage'
import { tooltipAt, type TooltipState } from '../lib/tooltip'
import { AgentDot, ChartTooltip, Segmented, TokenMixBar, TooltipSeriesRow } from './chart-kit'

const MIN_DAYS = 7
const MAX_DAYS = 30
const DAY_MS = 86_400_000
const USER_AGENT_PREVIEW = 3

export function AgentsView({ entries, onDrill }: { entries: UsageEntry[]; onDrill: (agentId: string, model?: string) => void }) {
  const usage = useMemo(() => usageByAgent(entries), [entries])
  return (
    <div className="space-y-6">
      <DailyAgentBars entries={entries} />
      <AgentTable usage={usage} onDrill={onDrill} />
    </div>
  )
}

function niceCeil(value: number): number {
  if (value <= 0) return 1
  const magnitude = 10 ** Math.floor(Math.log10(value))
  const fraction = value / magnitude
  const nice = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 2.5 ? 2.5 : fraction <= 5 ? 5 : 10
  return nice * magnitude
}

function DailyAgentBars({ entries }: { entries: UsageEntry[] }) {
  const [metric, setMetric] = useState<Metric>('tokens')
  const [tooltip, setTooltip] = useState<TooltipState>(null)
  const byDay = useMemo(() => usageByDay(entries), [entries])
  const todayKey = dayKey(new Date())

  const chart = useMemo(() => {
    const today = parseDayKey(todayKey)
    let earliest = todayKey
    for (const day of byDay.keys()) if (day < earliest) earliest = day
    const elapsed = Math.round((today.getTime() - parseDayKey(earliest).getTime()) / DAY_MS) + 1
    const span = Math.min(MAX_DAYS, Math.max(MIN_DAYS, elapsed))

    const agents = new Map<string, Agent>()
    for (const usage of byDay.values()) {
      for (const { agent } of usage.agents.values()) agents.set(agent.id, agent)
    }
    // Stack in palette slot order, the order the colors were validated in, so
    // an agent keeps the same position and hue no matter which others exist.
    const series = [...agents.values()].sort(compareAgents)

    const columns = Array.from({ length: span }, (_, index) => {
      const day = dayKey(addDays(today, index - span + 1))
      const usage = byDay.get(day)
      const segments = series.map((agent) => {
        const totals = usage?.agents.get(agent.id)?.totals
        return { agent, value: totals ? metricValue(totals, metric) : 0 }
      })
      return { day, segments, total: segments.reduce((sum, segment) => sum + segment.value, 0) }
    })
    const top = niceCeil(Math.max(0, ...columns.map((column) => column.total)))
    return { series, columns, top, span }
  }, [byDay, metric, todayKey])

  const tick = (value: number) => (metric === 'requests' ? formatInteger(value) : compact(value))

  function show(element: Element, column: (typeof chart.columns)[number]) {
    setTooltip(tooltipAt(element, <>
      <div className="font-semibold text-foreground">{formatMetric(column.total, metric)}</div>
      <div className="mb-1.5 text-muted-foreground">{formatDay(column.day)}</div>
      <div className="space-y-1">
        {column.segments.filter((segment) => segment.value > 0).map((segment) => (
          <TooltipSeriesRow key={segment.agent.id} color={segment.agent.color} label={segment.agent.name} value={tick(segment.value)} />
        ))}
      </div>
    </>))
  }

  return (
    <Card className="glass-surface min-w-0 rounded-[1.5rem]">
      <CardContent className="p-5 sm:p-6">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h2 className="text-base font-semibold tracking-tight">每日用量 · 按 Agent</h2>
            <p className="mt-1 text-sm text-muted-foreground">近 {chart.span} 天，悬停查看各 Agent 的数值</p>
          </div>
          <Segmented label="指标" value={metric} options={METRIC_OPTIONS} onChange={setMetric} />
        </div>

        {chart.series.length >= 2 && (
          <ul className="mt-4 flex flex-wrap gap-x-4 gap-y-1.5 text-xs text-muted-foreground" aria-label="图例">
            {chart.series.map((agent) => (
              <li key={agent.id} className="inline-flex items-center gap-1.5">
                <span aria-hidden className="size-2.5 rounded-[2px]" style={{ background: agent.color }} />
                {agent.name}
              </li>
            ))}
          </ul>
        )}

        {entries.length === 0 ? (
          <p className="py-16 text-center text-sm text-muted-foreground">暂无请求记录。</p>
        ) : (
          <div className="mt-5">
            <div className="flex">
              <div className="relative h-56 w-12 shrink-0" aria-hidden>
                {[0, 0.5, 1].map((ratio) => (
                  <span key={ratio} className="absolute right-2 translate-y-1/2 text-[0.68rem] tabular-nums text-muted-foreground" style={{ bottom: `${ratio * 100}%` }}>
                    {tick(chart.top * ratio)}
                  </span>
                ))}
              </div>
              <div className="relative h-56 flex-1">
                {[0, 0.5, 1].map((ratio) => (
                  <div key={ratio} aria-hidden className="glass-divider absolute inset-x-0 border-t" style={{ bottom: `${ratio * 100}%` }} />
                ))}
                <div className="absolute inset-0 flex items-end">
                  {chart.columns.map((column) => (
                    <div
                      key={column.day}
                      tabIndex={0}
                      aria-label={`${formatDay(column.day)}：${formatMetric(column.total, metric)}`}
                      className="flex h-full min-w-0 flex-1 items-end justify-center rounded-md px-[1px] outline-none hover:bg-muted focus-visible:bg-muted"
                      onMouseEnter={(event) => show(event.currentTarget, column)}
                      onMouseLeave={() => setTooltip(null)}
                      onFocus={(event) => show(event.currentTarget, column)}
                      onBlur={() => setTooltip(null)}
                    >
                      {column.total > 0 && (
                        // Short stacks drop the separating gaps, otherwise the gaps outweigh the data.
                        <div className={cn('flex w-full max-w-7 flex-col-reverse overflow-hidden rounded-t-[4px]', column.total / chart.top > 0.05 && 'gap-[2px]')} style={{ height: `${(column.total / chart.top) * 100}%` }}>
                          {column.segments.filter((segment) => segment.value > 0).map((segment) => (
                            <span key={segment.agent.id} className="min-h-px basis-0" style={{ flexGrow: segment.value, background: segment.agent.color }} />
                          ))}
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              </div>
            </div>
            <div className="ml-12 mt-1.5 flex" aria-hidden>
              {chart.columns.map((column, index) => {
                const date = parseDayKey(column.day)
                const labelled = chart.span <= 14 || (chart.span - 1 - index) % 5 === 0
                return (
                  <span key={column.day} className="min-w-0 flex-1 text-center text-[0.68rem] tabular-nums text-muted-foreground">
                    {labelled ? (index === 0 || date.getDate() === 1 ? `${date.getMonth() + 1}/${date.getDate()}` : date.getDate()) : ''}
                  </span>
                )
              })}
            </div>
          </div>
        )}
      </CardContent>
      <ChartTooltip state={tooltip} />
    </Card>
  )
}

function AgentTable({ usage, onDrill }: { usage: AgentUsage[]; onDrill: (agentId: string, model?: string) => void }) {
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set())
  const now = Date.now()

  function toggle(id: string) {
    setExpanded((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  return (
    <Card className="glass-surface overflow-hidden rounded-[1.5rem]">
      <CardContent className="p-0">
        <div className="glass-divider border-b px-5 py-5 sm:px-6">
          <h2 className="text-base font-semibold tracking-tight">Agent 与模型</h2>
          <p className="mt-1 text-sm text-muted-foreground">近一年，按 User-Agent 识别客户端；展开查看每个模型的用量，点「日志」跳到对应请求。</p>
        </div>
        <div className="overflow-x-auto">
          <table className="w-full min-w-[980px] text-left text-sm">
            <thead className="glass-divider border-b bg-white/18 text-xs font-medium tracking-[0.08em] text-muted-foreground uppercase dark:bg-white/3">
              <tr>
                <th className="px-5 py-3 sm:px-6">Agent</th>
                <th className="px-3 py-3 text-right">请求</th>
                <th className="px-3 py-3 text-right">成功率</th>
                <th className="px-3 py-3 text-right">Token</th>
                <th className="px-3 py-3 text-right">缓存命中</th>
                <th className="px-3 py-3">构成</th>
                <th className="px-3 py-3">最近使用</th>
                <th className="px-3 py-3 pr-5 sm:pr-6"><span className="sr-only">操作</span></th>
              </tr>
            </thead>
            <tbody className="glass-divider divide-y">
              {usage.map((item) => {
                const open = expanded.has(item.agent.id)
                return (
                  <Fragment key={item.agent.id}>
                    <tr className="glass-row">
                      <td className="px-5 py-3 sm:px-6">
                        <button type="button" aria-expanded={open} className="flex w-full cursor-pointer items-center gap-2.5 text-left outline-none focus-visible:ring-2 focus-visible:ring-ring rounded-md" onClick={() => toggle(item.agent.id)}>
                          <ChevronRight className={cn('size-4 shrink-0 text-muted-foreground transition-transform', open && 'rotate-90')} />
                          <AgentDot agent={item.agent} />
                          <span className="min-w-0">
                            <span className="block font-medium">{item.agent.name}</span>
                            <span className="block truncate text-xs text-muted-foreground">
                              {item.models.length} 个模型 · {item.models.slice(0, 2).map((model) => model.model || '(empty)').join('、')}
                            </span>
                          </span>
                        </button>
                      </td>
                      <UsageCells totals={item.totals} now={now} />
                      <td className="px-3 py-3 pr-5 text-right sm:pr-6">
                        <Button variant="ghost" size="xs" onClick={() => onDrill(item.agent.id)}>日志 <ArrowRight /></Button>
                      </td>
                    </tr>
                    {open && item.models.map((model) => (
                      <tr key={model.model} className="glass-row bg-muted/40">
                        <td className="py-2.5 pr-3 pl-[4.25rem]">
                          <span className="block truncate font-mono text-xs" title={model.model}>{model.model || '(empty)'}</span>
                        </td>
                        <UsageCells totals={model.totals} now={now} />
                        <td className="px-3 py-2.5 pr-5 text-right sm:pr-6">
                          {model.model && <Button variant="ghost" size="xs" onClick={() => onDrill(item.agent.id, model.model)}>日志 <ArrowRight /></Button>}
                        </td>
                      </tr>
                    ))}
                    {open && (
                      <tr className="bg-muted/40">
                        <td colSpan={8} className="pt-1 pr-5 pb-3 pl-[4.25rem] text-xs text-muted-foreground">
                          <span className="font-medium">User-Agent：</span>
                          <span className="font-mono break-all">{item.userAgents.slice(0, USER_AGENT_PREVIEW).join(' ｜ ') || '（空）'}</span>
                          {item.userAgents.length > USER_AGENT_PREVIEW && ` 等 ${item.userAgents.length} 种`}
                        </td>
                      </tr>
                    )}
                  </Fragment>
                )
              })}
            </tbody>
          </table>
          {usage.length === 0 && <div className="px-6 py-20 text-center text-sm text-muted-foreground">暂无请求记录。</div>}
        </div>
      </CardContent>
    </Card>
  )
}

function UsageCells({ totals, now }: { totals: Totals; now: number }) {
  return (
    <>
      <td className="px-3 py-3 text-right tabular-nums">
        <div>{formatInteger(totals.requests)}</div>
        {totals.attempts > totals.requests && <div className="text-xs text-muted-foreground">重试 {formatInteger(totals.attempts - totals.requests)}</div>}
      </td>
      <td className="px-3 py-3 text-right tabular-nums">{formatRate(successRate(totals))}</td>
      <td className="px-3 py-3 text-right tabular-nums">
        <div className="font-medium">{compact(totalTokens(totals))}</div>
        <div className="text-xs text-muted-foreground">入 {compact(totals.input)} · 出 {compact(totals.output)}</div>
      </td>
      <td className="px-3 py-3 text-right tabular-nums">{formatRate(cacheHitRate(totals))}</td>
      <td className="px-3 py-3"><TokenMixBar totals={totals} className="w-32" /></td>
      <td className="px-3 py-3 whitespace-nowrap text-muted-foreground">{totals.lastSeen > 0 ? relativeTime(totals.lastSeen, now) : '—'}</td>
    </>
  )
}
