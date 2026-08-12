import {
  ChevronLeft,
  ChevronRight,
  KeyRound,
  Moon,
  Search,
  Sun,
  Trash2,
  X,
} from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'

type Theme = 'light' | 'dark'

type NameCount = {
  name: string
  count: number
}

type Stats = {
  enabled: boolean
  total: number
  errors: number
  success: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  cache_hit_rate: number
  avg_duration_ms: number
  output_tps: number
  by_model: NameCount[]
  by_upstream: NameCount[]
  by_status: NameCount[]
  by_protocol: NameCount[]
}

type RequestLog = {
  id: number
  request_id: string
  timestamp: string
  method: string
  path: string
  status_code: number
  model: string
  protocol: string
  provider: string
  upstream: string
  user_agent: string
  duration_ms: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  error: string
  req_body?: string
  resp_body?: string
}

type LogList = {
  items: RequestLog[]
  total: number
  limit: number
  offset: number
}

type Filters = {
  model: string
  upstream: string
  protocol: string
  status: string
  errorsOnly: boolean
}

const API_KEY_STORAGE = 'lite-cpa-api-key'
const THEME_STORAGE = 'lite-cpa-theme'
const PAGE_SIZE = 50

class APIError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

function savedTheme(): Theme {
  return localStorage.getItem(THEME_STORAGE) === 'dark' ? 'dark' : 'light'
}

function savedAPIKey(): string {
  return localStorage.getItem(API_KEY_STORAGE) ?? ''
}

async function api<T>(path: string, init?: RequestInit): Promise<T> {
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

function appendLogFilters(query: URLSearchParams, filters: Filters): void {
  if (filters.model) query.set('model', filters.model)
  if (filters.upstream) query.set('upstream', filters.upstream)
  if (filters.protocol) query.set('protocol', filters.protocol)
  if (filters.status) query.set('status', filters.status)
  if (filters.errorsOnly) query.set('errors', '1')
}

function compact(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '0'
  // Force English locale so token counts always render as K/M/B,
  // regardless of the browser language (zh would show 万/亿 otherwise).
  return new Intl.NumberFormat('en', {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(value)
}

function formatInteger(value: number): string {
  return new Intl.NumberFormat().format(value || 0)
}

function formatRate(value: number | null): string {
  if (value === null || !Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(undefined, { style: 'percent', maximumFractionDigits: 1 }).format(value)
}

function requestTime(value: string): { date: string; time: string } {
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

function formatDateTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.valueOf()) ? value || '—' : date.toLocaleString()
}

function tokensPerSecond(record: RequestLog): string {
  if (record.output_tokens <= 0 || record.duration_ms <= 0) return '—'
  return ((record.output_tokens * 1000) / record.duration_ms).toFixed(1)
}

function statusTone(status: number): string {
  if (status >= 500) return 'bg-red-500/12 text-red-700 dark:text-red-300'
  if (status >= 400) return 'bg-amber-500/12 text-amber-700 dark:text-amber-300'
  if (status >= 200 && status < 300) return 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300'
  return 'bg-muted text-muted-foreground'
}

function MetricCard({ label, value, hint, tone }: { label: string; value: string; hint: string; tone?: string }) {
  return (
    <Card className="glass-surface rounded-[1.25rem]">
      <CardContent className="p-5">
        <p className="text-sm font-medium text-muted-foreground">{label}</p>
        <p className={`mt-3 text-3xl font-semibold tracking-tight ${tone ?? 'text-foreground'}`}>{value}</p>
        <p className="mt-1 text-xs text-muted-foreground">{hint}</p>
      </CardContent>
    </Card>
  )
}

function App() {
  const [theme, setTheme] = useState<Theme>(savedTheme)
  const [stats, setStats] = useState<Stats | null>(null)
  const [logs, setLogs] = useState<LogList>({ items: [], total: 0, limit: PAGE_SIZE, offset: 0 })
  const [filters, setFilters] = useState<Filters>({ model: '', upstream: '', protocol: '', status: '', errorsOnly: false })
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [upstreamOptions, setUpstreamOptions] = useState<string[]>([])
  const [statusOptions, setStatusOptions] = useState<string[]>([])
  const [draftFilters, setDraftFilters] = useState<Filters>(filters)
  const [isLoading, setIsLoading] = useState(true)
  const [status, setStatus] = useState('正在加载请求日志…')
  const [apiKeyOpen, setAPIKeyOpen] = useState(false)
  const [apiKey, setAPIKey] = useState(savedAPIKey)
  const [clearOpen, setClearOpen] = useState(false)
  const [isClearing, setIsClearing] = useState(false)
  const [selectedLog, setSelectedLog] = useState<RequestLog | null>(null)

  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark')
    localStorage.setItem(THEME_STORAGE, theme)
  }, [theme])

  // Track current offset so pagination clicks don't fight with filter-reset effects.
  const offsetRef = useRef(0)

  const load = useCallback(async (offset?: number) => {
    const actual = offset ?? offsetRef.current
    setIsLoading(true)
    const listQuery = new URLSearchParams({ limit: String(PAGE_SIZE), offset: String(actual) })
    const statsQuery = new URLSearchParams()
    appendLogFilters(listQuery, filters)
    appendLogFilters(statsQuery, filters)

    try {
      const [nextStats, nextLogs] = await Promise.all([
        api<Stats>(statsQuery.size ? `/api/logs/stats?${statsQuery}` : '/api/logs/stats'),
        api<LogList>(`/api/logs?${listQuery.toString()}`),
      ])
      setStats(nextStats)
      setModelOptions((current) => {
        const names = new Set(current)
        let changed = false
        for (const { name } of nextStats.by_model) {
          if (name && !names.has(name)) {
            names.add(name)
            changed = true
          }
        }
        return changed ? [...names].sort((a, b) => a.localeCompare(b)) : current
      })
      setUpstreamOptions((current) => {
        const names = new Set(current)
        let changed = false
        for (const { name } of nextStats.by_upstream) {
          if (name && !names.has(name)) {
            names.add(name)
            changed = true
          }
        }
        return changed ? [...names].sort((a, b) => a.localeCompare(b)) : current
      })
      setStatusOptions((current) => {
        const codes = new Set(current)
        let changed = false
        for (const { name } of nextStats.by_status) {
          // Group status codes into "2xx", "4xx", "5xx" etc.
          if (name) {
            const group = name[0] + 'xx'
            if (!codes.has(group)) {
              codes.add(group)
              changed = true
            }
          }
        }
        return changed ? [...codes].sort() : current
      })
      setLogs(nextLogs)
      offsetRef.current = nextLogs.offset
      setStatus(nextStats.enabled ? `已启用 · 更新于 ${new Date().toLocaleTimeString()}` : 'request-log 未启用')
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        setStatus('请输入网关 API Key 以查看请求日志。')
        setAPIKeyOpen(true)
      } else {
        setStatus(error instanceof Error ? `加载失败：${error.message}` : '加载失败')
      }
    } finally {
      setIsLoading(false)
    }
  }, [filters])

  useEffect(() => {
    offsetRef.current = 0
    void load(0)
  }, [filters, load])

  // Auto-refresh every 5 seconds.
  useEffect(() => {
    const timer = window.setInterval(() => void load(), 5000)
    return () => window.clearInterval(timer)
  }, [load])

  const models = modelOptions.includes(draftFilters.model) || !draftFilters.model
    ? modelOptions
    : [...modelOptions, draftFilters.model].sort((a, b) => a.localeCompare(b))

  const upstreams = upstreamOptions.includes(draftFilters.upstream) || !draftFilters.upstream
    ? upstreamOptions
    : [...upstreamOptions, draftFilters.upstream].sort((a, b) => a.localeCompare(b))

  const statuses = statusOptions.length > 0
    ? statusOptions
    : []

  const currentFrom = logs.total === 0 ? 0 : logs.offset + 1
  const currentTo = Math.min(logs.offset + logs.items.length, logs.total)

  function applyFilters(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setFilters(draftFilters)
  }

  function saveAPIKey(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const key = apiKey.trim()
    if (key) localStorage.setItem(API_KEY_STORAGE, key)
    else localStorage.removeItem(API_KEY_STORAGE)
    setAPIKey(key)
    setAPIKeyOpen(false)
    void load(0)
  }

  async function clearLogs() {
    setIsClearing(true)
    try {
      const result = await api<{ deleted: number }>('/api/logs', { method: 'DELETE' })
      setClearOpen(false)
      setStatus(`已清除 ${result.deleted} 条请求日志 · ${new Date().toLocaleTimeString()}`)
      await load(0)
    } catch (error) {
      setStatus(error instanceof Error ? `清除失败：${error.message}` : '清除失败')
    } finally {
      setIsClearing(false)
    }
  }

  return (
    <main className="apple-canvas min-h-screen overflow-x-hidden px-4 py-5 pb-[max(1.25rem,env(safe-area-inset-bottom))] text-foreground sm:px-7 sm:py-8">
      <div className="mx-auto max-w-[1440px]">
        <header className="glass-surface glass-header mb-6 flex items-center justify-between rounded-[1.5rem] px-5 py-4 sm:px-6">
          <div className="flex min-w-0 items-center gap-3">
            <span className="grid size-10 shrink-0 place-items-center rounded-[0.9rem] bg-gradient-to-br from-blue-500 via-indigo-500 to-violet-500 shadow-lg shadow-blue-500/20">
              <span className="size-3 rounded-full border-2 border-white/95 bg-white/35" />
            </span>
            <div className="min-w-0">
              <h1 className="truncate text-lg font-semibold tracking-tight sm:text-xl">lite-cpa <span className="text-muted-foreground">/ 请求日志</span></h1>
              <p className="mt-0.5 truncate text-xs text-muted-foreground" aria-live="polite">{status}</p>
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Button variant="ghost" size="icon-lg" aria-label="设置 API Key" title="设置 API Key" onClick={() => setAPIKeyOpen(true)}>
              <KeyRound />
            </Button>
            <Button variant="ghost" size="icon-lg" aria-label="切换昼夜主题" title="切换昼夜主题" onClick={() => setTheme((value) => value === 'dark' ? 'light' : 'dark')}>
              {theme === 'dark' ? <Sun /> : <Moon />}
            </Button>
          </div>
        </header>

        {stats && !stats.enabled && (
          <div className="glass-notice mb-6 rounded-[1.1rem] px-4 py-3 text-sm text-amber-900 dark:text-amber-100">
            request-log 未启用。请在 <code className="rounded bg-black/5 px-1.5 py-0.5 dark:bg-white/10">config.yaml</code> 中设置 <code className="rounded bg-black/5 px-1.5 py-0.5 dark:bg-white/10">request-log.enabled: true</code> 后重启。
          </div>
        )}

        <section className="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-5" aria-label="日志统计">
          <MetricCard label="总请求" value={formatInteger(stats?.total ?? 0)} hint="全部已记录请求" />
          <MetricCard label="输入 Token" value={compact(stats?.input_tokens ?? 0)} hint={formatInteger(stats?.input_tokens ?? 0)} />
          <MetricCard label="输出 Token" value={compact(stats?.output_tokens ?? 0)} hint={formatInteger(stats?.output_tokens ?? 0)} />
          <MetricCard label="成功率" value={formatRate((stats?.total ?? 0) > 0 ? (stats?.success ?? 0) / (stats?.total ?? 1) : null)} hint={`${formatInteger(stats?.success ?? 0)} / ${formatInteger(stats?.total ?? 0)} 成功`} tone="text-emerald-700 dark:text-emerald-300" />
          <MetricCard label="缓存命中率" value={formatRate((stats?.input_tokens ?? 0) > 0 ? stats?.cache_hit_rate ?? 0 : null)} hint={`${formatInteger(stats?.cached_tokens ?? 0)} / ${formatInteger(stats?.input_tokens ?? 0)} 缓存 Token`} tone="text-sky-700 dark:text-sky-300" />
        </section>

        <Card className="glass-surface overflow-hidden rounded-[1.5rem]">
          <CardContent className="p-0">
            <div className="glass-divider flex flex-col gap-4 border-b px-5 py-5 sm:px-6 lg:flex-row lg:items-center lg:justify-between">
              <div>
                <h2 className="text-base font-semibold tracking-tight">请求日志</h2>
                <p className="mt-1 text-sm text-muted-foreground">按模型、上游和协议筛选请求。点击行查看完整记录。</p>
              </div>
              <Button variant="destructive-outline" className="min-h-10 sm:min-h-10" disabled={!stats?.enabled || logs.total === 0} onClick={() => setClearOpen(true)}>
                <Trash2 /> 清除日志
              </Button>
            </div>

            <form className="glass-divider grid gap-3 border-b p-5 sm:grid-cols-2 lg:grid-cols-[minmax(10rem,1fr)_minmax(10rem,1fr)_minmax(8rem,1fr)_9rem_auto_auto] lg:items-center sm:px-6" onSubmit={applyFilters}>
              <label className="sr-only" htmlFor="model">模型</label>
              <select id="model" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={draftFilters.model} onChange={(event) => setDraftFilters((value) => ({ ...value, model: event.target.value }))}>
                <option value="">所有模型</option>
                {models.map((model) => <option key={model} value={model}>{model}</option>)}
              </select>
              <label className="sr-only" htmlFor="upstream">上游</label>
              <select id="upstream" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={draftFilters.upstream} onChange={(event) => setDraftFilters((value) => ({ ...value, upstream: event.target.value }))}>
                <option value="">所有上游</option>
                {upstreams.map((upstream) => <option key={upstream} value={upstream}>{upstream}</option>)}
              </select>
              <label className="sr-only" htmlFor="status">状态码</label>
              <select id="status" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={draftFilters.status} onChange={(event) => setDraftFilters((value) => ({ ...value, status: event.target.value }))}>
                <option value="">所有状态码</option>
                {statuses.map((status) => <option key={status} value={status}>{status}</option>)}
              </select>
              <label className="sr-only" htmlFor="protocol">协议</label>
              <select id="protocol" className="glass-select w-full px-3 text-sm text-foreground outline-none" value={draftFilters.protocol} onChange={(event) => setDraftFilters((value) => ({ ...value, protocol: event.target.value }))}>
                <option value="">所有协议</option>
                <option value="chat">chat</option>
                <option value="responses">responses</option>
                <option value="claude">claude</option>
              </select>
              <label className="flex min-h-10 cursor-pointer items-center gap-2 whitespace-nowrap px-1 text-sm text-muted-foreground">
                <input type="checkbox" checked={draftFilters.errorsOnly} onChange={(event) => setDraftFilters((value) => ({ ...value, errorsOnly: event.target.checked }))} />
                仅错误
              </label>
              <Button className="min-h-10 sm:min-h-10" type="submit"><Search /> 筛选</Button>
            </form>

            <div className="overflow-x-auto">
              <table className="w-full min-w-[920px] text-left text-sm">
                <thead className="glass-divider border-b bg-white/18 text-xs font-medium uppercase tracking-[0.08em] text-muted-foreground dark:bg-white/3">
                  <tr>
                    <th className="px-5 py-3 sm:px-6">时间</th>
                    <th className="px-3 py-3">状态</th>
                    <th className="px-3 py-3">输入 / 输出 / 缓存</th>
                    <th className="px-3 py-3">TPS</th>
                    <th className="px-3 py-3">模型</th>
                    <th className="px-3 py-3">上游</th>
                    <th className="px-3 py-3 pr-5 sm:pr-6">协议</th>
                  </tr>
                </thead>
                <tbody className="glass-divider divide-y">
                  {logs.items.map((record) => {
                    const time = requestTime(record.timestamp)
                    return (
                      <tr key={record.id} className="glass-row cursor-pointer" onClick={() => setSelectedLog(record)}>
                        <td className="px-5 py-3 align-top font-mono text-xs sm:px-6"><div>{time.date}</div><div className="mt-0.5 text-muted-foreground">{time.time}</div></td>
                        <td className="px-3 py-3 align-top"><span className={`rounded-full px-2 py-1 text-xs font-semibold ${statusTone(record.status_code)}`} title={record.error}>{record.status_code}</span></td>
                        <td className="px-3 py-3 align-top font-mono text-xs"><span>{compact(record.input_tokens)}</span><span className="px-1 text-muted-foreground">/</span><span>{compact(record.output_tokens)}</span><span className="px-1 text-muted-foreground">/</span><span className="text-muted-foreground">{compact(record.cached_tokens)}</span></td>
                        <td className="px-3 py-3 align-top font-mono text-xs">{tokensPerSecond(record)} <span className="text-muted-foreground">tok/s</span></td>
                        <td className="max-w-56 truncate px-3 py-3 align-top font-mono text-xs" title={record.model}>{record.model || '—'}</td>
                        <td className="max-w-48 truncate px-3 py-3 align-top font-mono text-xs" title={record.upstream}>{record.upstream || '—'}</td>
                        <td className="px-3 py-3 pr-5 align-top sm:pr-6"><span className="rounded bg-muted px-2 py-1 font-mono text-xs text-muted-foreground">{record.protocol || '—'}</span></td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
              {!isLoading && logs.items.length === 0 && <div className="px-6 py-20 text-center text-sm text-muted-foreground">暂无请求记录。</div>}
            </div>

            <div className="glass-divider flex items-center justify-between gap-4 border-t px-5 py-4 sm:px-6">
              <span className="text-sm text-muted-foreground">{currentFrom}–{currentTo} / {logs.total}</span>
              <div className="flex gap-2">
                <Button variant="outline" size="sm" disabled={logs.offset <= 0 || isLoading} onClick={() => void load(Math.max(0, logs.offset - PAGE_SIZE))}><ChevronLeft /> 上一页</Button>
                <Button variant="outline" size="sm" disabled={logs.offset + logs.limit >= logs.total || isLoading} onClick={() => void load(logs.offset + PAGE_SIZE)}>下一页 <ChevronRight /></Button>
              </div>
            </div>
          </CardContent>
        </Card>
      </div>

      {apiKeyOpen && <Modal title="Gateway API Key" onClose={() => setAPIKeyOpen(false)}>
        <form className="space-y-5" onSubmit={saveAPIKey}>
          <p className="text-sm leading-6 text-muted-foreground">输入网关 Bearer Token 后才能读取请求日志。密钥仅保存在当前浏览器中。</p>
          <Input type="password" autoFocus value={apiKey} placeholder="sk-..." onChange={(event) => setAPIKey(event.target.value)} />
          <div className="flex justify-end gap-2"><Button variant="outline" onClick={() => setAPIKeyOpen(false)}>取消</Button><Button type="submit">保存</Button></div>
        </form>
      </Modal>}

      {clearOpen && <Modal title="清除所有请求日志？" onClose={() => !isClearing && setClearOpen(false)}>
        <p className="text-sm leading-6 text-muted-foreground">该操作会永久删除所有已存储的请求日志，无法撤销。</p>
        <div className="mt-6 flex justify-end gap-2"><Button variant="outline" disabled={isClearing} onClick={() => setClearOpen(false)}>取消</Button><Button variant="destructive" loading={isClearing} onClick={() => void clearLogs()}>清除全部</Button></div>
      </Modal>}

      {selectedLog && <Modal title="请求详情" onClose={() => setSelectedLog(null)} wide>
        <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-[9rem_1fr]">
          <Detail label="request_id" value={selectedLog.request_id} />
          <Detail label="time" value={formatDateTime(selectedLog.timestamp)} />
          <Detail label="status" value={`${selectedLog.status_code} · ${selectedLog.duration_ms} ms`} />
          <Detail label="input tokens" value={formatInteger(selectedLog.input_tokens)} />
          <Detail label="output tokens" value={formatInteger(selectedLog.output_tokens)} />
          <Detail label="TPS" value={tokensPerSecond(selectedLog)} />
          <Detail label="model" value={selectedLog.model || '—'} />
          <Detail label="upstream" value={`${selectedLog.upstream || '—'} (${selectedLog.provider || '—'})`} />
          <Detail label="protocol" value={selectedLog.protocol || '—'} />
          <Detail label="path" value={`${selectedLog.method} ${selectedLog.path}`} />
          <Detail label="user-agent" value={selectedLog.user_agent || '—'} />
          <Detail label="error" value={selectedLog.error || '—'} />
        </dl>
        {(selectedLog.req_body || selectedLog.resp_body) ? <div className="mt-6 space-y-5"><Payload title="Request body" value={selectedLog.req_body || '(empty)'} /><Payload title="Response body" value={selectedLog.resp_body || '(empty / stream omitted)'} /></div> : <p className="mt-6 text-sm text-muted-foreground">请求与响应正文未存储（store-body: false）。</p>}
      </Modal>}
    </main>
  )
}

function Modal({ children, onClose, title, wide = false }: { children: React.ReactNode; onClose: () => void; title: string; wide?: boolean }) {
  return <div className="fixed inset-0 z-50 grid place-items-center bg-slate-950/42 p-4 backdrop-blur-md dark:bg-black/58" role="presentation" onMouseDown={onClose}>
    <Card className={`glass-modal max-h-[calc(100vh-2rem)] w-full overflow-auto rounded-[1.5rem] ${wide ? 'max-w-3xl' : 'max-w-md'}`} role="dialog" aria-modal="true" aria-label={title} onMouseDown={(event) => event.stopPropagation()}>
      <CardContent className="p-5 sm:p-6"><div className="mb-5 flex items-center justify-between gap-4"><h2 className="text-lg font-semibold tracking-tight">{title}</h2><Button variant="ghost" size="icon-sm" aria-label="关闭" onClick={onClose}><X /></Button></div>{children}</CardContent>
    </Card>
  </div>
}

function Detail({ label, value }: { label: string; value: string }) {
  return <><dt className="font-mono text-xs text-muted-foreground">{label}</dt><dd className="break-words font-mono text-xs leading-5 text-foreground">{value}</dd></>
}

function Payload({ title, value }: { title: string; value: string }) {
  return <section><h3 className="mb-2 text-sm font-medium">{title}</h3><pre className="max-h-64 overflow-auto rounded-xl bg-muted p-4 text-xs leading-5 whitespace-pre-wrap break-words">{value}</pre></section>
}

export default App
