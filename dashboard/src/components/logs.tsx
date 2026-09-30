import { ChevronLeft, ChevronRight, RotateCcw, Trash2, X } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import type { LogPage, LogStats, NameCount, UsageMeta } from '../lib/api'
import {
  displayedTokens,
  formatDay,
  httpStatus,
  outcomeLabel,
  outcomeTone,
  requestTime,
  tokensPerSecond,
} from '../lib/format'
import { appendFieldFilters, EMPTY_FILTERS, logQuery, RANGE_OPTIONS, type LogFilters, type LogRange } from '../lib/log-filters'
import { enrichLog, type LogRecord } from '../lib/usage'
import { usePolling } from '../lib/use-polling'
import { AgentDot } from './chart-kit'

const PAGE_SIZE = 50

function names(counts: NameCount[] | undefined, selected: string): string[] {
  const set = new Set<string>()
  for (const { name } of counts ?? []) if (name && name !== '(empty)') set.add(name)
  if (selected) set.add(selected)
  return [...set].sort((a, b) => a.localeCompare(b))
}

function statusGroups(counts: NameCount[] | undefined, selected: string): string[] {
  const set = new Set<string>()
  for (const { name } of counts ?? []) if (name) set.add(name === '0' ? '0' : `${name[0]}xx`)
  if (selected) set.add(selected)
  return [...set].sort()
}

function retentionNote(meta: UsageMeta | null): string {
  if (!meta) return ''
  const days = (seconds: number) => `${Math.round((seconds / 86400) * 10) / 10} 天`
  const rows = meta.retention_seconds > 0 ? `原始日志保留 ${days(meta.retention_seconds)}` : '原始日志永久保留'
  if (!meta.store_body) return `${rows}，未存储正文`
  return meta.body_retention_seconds > 0 ? `${rows}，正文保留 ${days(meta.body_retention_seconds)}` : `${rows}，正文永久保留`
}

export function LogsView({ filters, onFiltersChange, userAgents, meta, version, canClear, onRequestClear, onSelect }: {
  filters: LogFilters
  onFiltersChange: (filters: LogFilters) => void
  // null means every agent; an empty list means the selected agent has no known User-Agent.
  userAgents: string[] | null
  meta: UsageMeta | null
  version: unknown
  canClear: boolean
  onRequestClear: () => void
  onSelect: (record: LogRecord) => void
}) {
  const [offset, setOffset] = useState(0)
  const blocked = userAgents !== null && userAgents.length === 0
  const scope = logQuery(filters, userAgents)
  const listQuery = appendFieldFilters(new URLSearchParams(scope), filters)
  listQuery.set('limit', String(PAGE_SIZE))
  listQuery.set('offset', String(offset))
  const list = usePolling<LogPage>(blocked ? null : `/api/logs?${listQuery}`, 5_000, version)
  const stats = usePolling<LogStats>(blocked ? null : `/api/logs/stats?${scope}`, 60_000, version)

  const items = useMemo(() => (blocked ? [] : (list.data?.items ?? []).map(enrichLog)), [blocked, list.data])
  const total = blocked ? 0 : (list.data?.total ?? 0)
  const models = names(stats.data?.by_model, filters.model)
  const upstreams = names(stats.data?.by_upstream, filters.upstream)
  const statuses = statusGroups(stats.data?.by_status, filters.status)
  const from = items.length === 0 ? 0 : offset + 1
  const to = offset + items.length
  const hasFilters = Boolean(filters.model || filters.upstream || filters.protocol || filters.status || filters.outcome || filters.day || filters.range !== EMPTY_FILTERS.range)

  function update(patch: Partial<LogFilters>) {
    onFiltersChange({ ...filters, ...patch })
    setOffset(0)
  }

  return (
    <Card className="glass-surface overflow-hidden rounded-[1.5rem]">
      <CardContent className="p-0">
        <div className="glass-divider flex flex-col gap-4 border-b px-5 py-5 sm:px-6 lg:flex-row lg:items-center lg:justify-between">
          <div>
            <h2 className="text-base font-semibold tracking-tight">请求日志</h2>
            <p className="mt-1 text-sm text-muted-foreground">
              每次上游尝试一行，筛选即时生效，点击行查看完整记录。{retentionNote(meta) && ` ${retentionNote(meta)}。`}
            </p>
          </div>
          <Button variant="destructive-outline" className="min-h-10 sm:min-h-10" disabled={!canClear} onClick={onRequestClear}>
            <Trash2 /> 清除日志
          </Button>
        </div>

        <div className="glass-divider grid gap-3 border-b p-5 sm:grid-cols-2 sm:px-6 lg:grid-cols-[8rem_minmax(9rem,1fr)_minmax(9rem,1fr)_8rem_8rem_8rem_auto] lg:items-center">
          <label className="sr-only" htmlFor="range">时间范围</label>
          <select id="range" className="glass-select w-full px-3 text-sm text-foreground outline-none disabled:opacity-60" value={filters.range} disabled={Boolean(filters.day)} onChange={(event) => update({ range: event.target.value as LogRange })}>
            {RANGE_OPTIONS.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
          </select>
          <label className="sr-only" htmlFor="model">模型</label>
          <select id="model" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={filters.model} onChange={(event) => update({ model: event.target.value })}>
            <option value="">所有模型</option>
            {models.map((model) => <option key={model} value={model}>{model}</option>)}
          </select>
          <label className="sr-only" htmlFor="upstream">上游</label>
          <select id="upstream" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={filters.upstream} onChange={(event) => update({ upstream: event.target.value })}>
            <option value="">所有上游</option>
            {upstreams.map((upstream) => <option key={upstream} value={upstream}>{upstream}</option>)}
          </select>
          <label className="sr-only" htmlFor="status">状态码</label>
          <select id="status" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={filters.status} onChange={(event) => update({ status: event.target.value })}>
            <option value="">所有状态码</option>
            {statuses.map((status) => <option key={status} value={status}>{status === '0' ? '未提交' : status}</option>)}
          </select>
          <label className="sr-only" htmlFor="protocol">协议</label>
          <select id="protocol" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={filters.protocol} onChange={(event) => update({ protocol: event.target.value })}>
            <option value="">所有协议</option>
            <option value="chat">chat</option>
            <option value="responses">responses</option>
            <option value="claude">claude</option>
          </select>
          <label className="sr-only" htmlFor="outcome">结果</label>
          <select id="outcome" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={filters.outcome} onChange={(event) => update({ outcome: event.target.value })}>
            <option value="">所有结果</option>
            <option value="completed">成功</option>
            <option value="error">错误</option>
            <option value="client_canceled">客户端取消</option>
            <option value="unknown">未知</option>
          </select>
          <Button variant="outline" className="min-h-10 sm:min-h-10" disabled={!hasFilters} onClick={() => update({ ...EMPTY_FILTERS })}>
            <RotateCcw /> 重置
          </Button>
          {filters.day && (
            <div className="sm:col-span-2 lg:col-span-7">
              <button type="button" className="inline-flex cursor-pointer items-center gap-1.5 rounded-full bg-muted px-3 py-1 text-xs font-medium text-foreground hover:bg-accent" onClick={() => update({ day: '' })}>
                日期：{formatDay(filters.day)} <X className="size-3.5" aria-label="移除日期筛选" />
              </button>
            </div>
          )}
        </div>

        <div className="overflow-x-auto">
          <table className="w-full min-w-[1040px] text-left text-sm">
            <thead className="glass-divider border-b bg-white/18 text-xs font-medium tracking-[0.08em] text-muted-foreground uppercase dark:bg-white/3">
              <tr>
                <th className="px-5 py-3 sm:px-6">时间</th>
                <th className="px-3 py-3">Agent</th>
                <th className="px-3 py-3">结果 / HTTP</th>
                <th className="px-3 py-3">输入 / 输出 / 缓存</th>
                <th className="px-3 py-3">TPS</th>
                <th className="px-3 py-3">模型</th>
                <th className="px-3 py-3">上游</th>
                <th className="px-3 py-3 pr-5 sm:pr-6">协议</th>
              </tr>
            </thead>
            <tbody className="glass-divider divide-y">
              {items.map((record) => {
                const time = requestTime(record.timestamp)
                return (
                  <tr key={record.id} className="glass-row cursor-pointer" onClick={() => onSelect(record)}>
                    <td className="px-5 py-3 align-top font-mono text-xs sm:px-6"><div>{time.date}</div><div className="mt-0.5 text-muted-foreground">{time.time}</div></td>
                    <td className="px-3 py-3 align-top text-xs" title={record.user_agent || '（空 User-Agent）'}>
                      <span className="inline-flex items-center gap-1.5 whitespace-nowrap"><AgentDot agent={record.agent} className="size-2" />{record.agent.name}</span>
                    </td>
                    <td className="px-3 py-3 align-top">
                      <span className={`inline-block whitespace-nowrap rounded-full px-2 py-1 text-xs font-semibold ${outcomeTone(record.outcome)}`} title={record.error}>{outcomeLabel(record.outcome)}</span>
                      {record.retried && <span className="ml-1.5 inline-block whitespace-nowrap rounded-full bg-muted px-2 py-1 text-[0.68rem] text-muted-foreground" title="这次尝试失败后，同一请求改用了其它上游">已重试</span>}
                      <div className="mt-1 font-mono text-[0.7rem] text-muted-foreground">HTTP {httpStatus(record.status_code)}</div>
                    </td>
                    <td className="px-3 py-3 align-top font-mono text-xs">
                      <div><span>{displayedTokens(record.input_tokens, record.usage_complete)}</span><span className="px-1 text-muted-foreground">/</span><span>{displayedTokens(record.output_tokens, record.usage_complete)}</span><span className="px-1 text-muted-foreground">/</span><span className="text-muted-foreground">{displayedTokens(record.cached_tokens, record.usage_complete)}</span></div>
                      {!record.usage_complete && !record.retried && <div className="mt-1 text-[0.7rem] text-amber-700 dark:text-amber-300">usage 未完整</div>}
                    </td>
                    <td className="px-3 py-3 align-top font-mono text-xs">{tokensPerSecond(record)} <span className="text-muted-foreground">tok/s</span></td>
                    <td className="max-w-56 truncate px-3 py-3 align-top font-mono text-xs" title={record.model}>{record.model || '—'}</td>
                    <td className="max-w-48 truncate px-3 py-3 align-top font-mono text-xs" title={record.upstream}>{record.upstream || '—'}</td>
                    <td className="px-3 py-3 pr-5 align-top sm:pr-6"><span className="rounded bg-muted px-2 py-1 font-mono text-xs text-muted-foreground">{record.protocol || '—'}</span></td>
                  </tr>
                )
              })}
            </tbody>
          </table>
          {items.length === 0 && (
            <div className="px-6 py-20 text-center text-sm text-muted-foreground">
              {list.error && !blocked ? `加载失败：${list.error.message}` : list.loading ? '正在加载…' : hasFilters || userAgents ? '没有符合筛选条件的请求。' : '暂无请求记录。'}
            </div>
          )}
        </div>

        <div className="glass-divider flex items-center justify-between gap-4 border-t px-5 py-4 sm:px-6">
          <span className="text-sm text-muted-foreground">{from}–{to} / {total}</span>
          <div className="flex gap-2">
            <Button variant="outline" size="sm" disabled={offset <= 0} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}><ChevronLeft /> 上一页</Button>
            <Button variant="outline" size="sm" disabled={offset + PAGE_SIZE >= total} onClick={() => setOffset(offset + PAGE_SIZE)}>下一页 <ChevronRight /></Button>
          </div>
        </div>
      </CardContent>
    </Card>
  )
}
