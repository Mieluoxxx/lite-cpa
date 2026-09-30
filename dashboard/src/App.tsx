import { Bot, KeyRound, LayoutDashboard, Moon, ScrollText, Sun } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Tabs, TabsList, TabsPanel, TabsTab } from '@/components/ui/tabs'
import { cn } from '@/lib/utils'
import { AgentsView } from './components/agents'
import { AgentDot } from './components/chart-kit'
import { LogsView } from './components/logs'
import { Detail, Modal, Payload } from './components/modal'
import { OverviewView } from './components/overview'
import { compareAgents } from './lib/agents'
import { api, API_KEY_STORAGE, APIError, savedAPIKey, type DailyUsageRow, type UsageResponse } from './lib/api'
import { EMPTY_FILTERS, type LogFilters } from './lib/log-filters'
import {
  dayKey,
  detailedTokens,
  formatDateTime,
  formatInteger,
  httpStatus,
  outcomeLabel,
  tokensPerSecond,
} from './lib/format'
import { enrichUsage, usageByAgent, type LogRecord } from './lib/usage'
import { usePolling } from './lib/use-polling'

type Theme = 'light' | 'dark'
type Tab = 'overview' | 'agents' | 'logs'

const THEME_STORAGE = 'lite-cpa-theme'
const TABS: Tab[] = ['overview', 'agents', 'logs']
const USAGE_DAYS = 371 // 53 weeks, the span the calendar heatmap renders
// UTC offset at load time; DST switches shift at most an hour until reload.
const TZ_OFFSET_MINUTES = -new Date().getTimezoneOffset()

function savedTheme(): Theme {
  return localStorage.getItem(THEME_STORAGE) === 'dark' ? 'dark' : 'light'
}

function tabFromHash(): Tab {
  const hash = window.location.hash.slice(1)
  return TABS.find((tab) => tab === hash) ?? 'overview'
}

function App() {
  const [theme, setTheme] = useState<Theme>(savedTheme)
  const [tab, setTab] = useState<Tab>(tabFromHash)
  const [authVersion, setAuthVersion] = useState(0)
  const [logsVersion, setLogsVersion] = useState(0)
  const usage = usePolling<UsageResponse<DailyUsageRow>>(`/api/usage?bucket=day&days=${USAGE_DAYS}&tz=${TZ_OFFSET_MINUTES}`, 60_000, authVersion)
  const meta = usage.data?.meta ?? null
  const [agentId, setAgentId] = useState<string | null>(null)
  const [selectedDay, setSelectedDay] = useState(() => dayKey(new Date()))
  const [logFilters, setLogFilters] = useState<LogFilters>(EMPTY_FILTERS)
  const [apiKeyOpen, setAPIKeyOpen] = useState(false)
  const [apiKey, setAPIKey] = useState(savedAPIKey)
  const [clearOpen, setClearOpen] = useState(false)
  const [isClearing, setIsClearing] = useState(false)
  const [notice, setNotice] = useState('')
  const [selectedLog, setSelectedLog] = useState<LogRecord | null>(null)

  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark')
    localStorage.setItem(THEME_STORAGE, theme)
  }, [theme])

  useEffect(() => {
    window.history.replaceState(null, '', `#${tab}`)
  }, [tab])

  const unauthorized = usage.error instanceof APIError && usage.error.status === 401
  useEffect(() => {
    if (unauthorized) setAPIKeyOpen(true)
  }, [unauthorized])

  useEffect(() => {
    if (!notice) return
    const timer = window.setTimeout(() => setNotice(''), 5000)
    return () => window.clearTimeout(timer)
  }, [notice])

  const allEntries = useMemo(() => enrichUsage(usage.data?.items ?? []), [usage.data])
  const agents = useMemo(
    () => usageByAgent(allEntries).sort((a, b) => compareAgents(a.agent, b.agent)),
    [allEntries],
  )
  const entries = useMemo(
    () => (agentId ? allEntries.filter((entry) => entry.agent.id === agentId) : allEntries),
    [allEntries, agentId],
  )
  const selectedUserAgents = useMemo(
    () => (agentId ? (agents.find((item) => item.agent.id === agentId)?.userAgents ?? []) : null),
    [agents, agentId],
  )
  const scope = useMemo(
    () => ({ days: USAGE_DAYS, tz: TZ_OFFSET_MINUTES, agentId, version: authVersion }),
    [agentId, authVersion],
  )
  const totalRequests = agents.reduce((sum, item) => sum + item.totals.requests, 0)

  let status = '正在加载用量数据…'
  if (notice) status = notice
  else if (unauthorized) status = '请输入网关 API Key 以查看用量与请求日志。'
  else if (usage.error) status = `加载失败：${usage.error.message}`
  else if (meta && !meta.enabled) status = 'request-log 未启用'
  else if (usage.syncedAt) status = `已启用 · 近一年 ${formatInteger(totalRequests)} 个请求 · 同步于 ${new Date(usage.syncedAt).toLocaleTimeString()}`

  function drillDay(day: string) {
    setLogFilters({ ...EMPTY_FILTERS, day })
    setTab('logs')
  }

  function drillAgent(id: string, model?: string) {
    setAgentId(id)
    setLogFilters({ ...EMPTY_FILTERS, model: model ?? '' })
    setTab('logs')
  }

  function saveAPIKey(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const key = apiKey.trim()
    if (key) localStorage.setItem(API_KEY_STORAGE, key)
    else localStorage.removeItem(API_KEY_STORAGE)
    setAPIKey(key)
    setAPIKeyOpen(false)
    setAuthVersion((version) => version + 1)
  }

  async function clearLogs() {
    setIsClearing(true)
    try {
      const result = await api<{ deleted: number }>('/api/logs', { method: 'DELETE' })
      setClearOpen(false)
      setNotice(`已清除 ${result.deleted} 条请求日志 · ${new Date().toLocaleTimeString()}`)
      setLogsVersion((version) => version + 1)
    } catch (error) {
      setNotice(error instanceof Error ? `清除失败：${error.message}` : '清除失败')
    } finally {
      setIsClearing(false)
    }
  }

  return (
    <main className="apple-canvas min-h-screen overflow-x-hidden px-4 py-5 pb-[max(1.25rem,env(safe-area-inset-bottom))] text-foreground sm:px-7 sm:py-8">
      <div className="mx-auto max-w-[1440px]">
        <header className="glass-surface glass-header mb-5 flex items-center justify-between rounded-[1.5rem] px-5 py-4 sm:px-6">
          <div className="flex min-w-0 items-center gap-3">
            <span className="grid size-10 shrink-0 place-items-center rounded-[0.9rem] bg-gradient-to-br from-blue-500 via-indigo-500 to-violet-500 shadow-lg shadow-blue-500/20">
              <span className="size-3 rounded-full border-2 border-white/95 bg-white/35" />
            </span>
            <div className="min-w-0">
              <h1 className="truncate text-lg font-semibold tracking-tight sm:text-xl">lite-cpa <span className="text-muted-foreground">/ 用量看板</span></h1>
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

        {meta && !meta.enabled && (
          <div className="glass-notice mb-5 rounded-[1.1rem] px-4 py-3 text-sm text-amber-900 dark:text-amber-100">
            request-log 未启用。请在 <code className="rounded bg-black/5 px-1.5 py-0.5 dark:bg-white/10">config.yaml</code> 中设置 <code className="rounded bg-black/5 px-1.5 py-0.5 dark:bg-white/10">request-log.enabled: true</code> 后重启。
          </div>
        )}

        <Tabs value={tab} onValueChange={(value) => setTab(value as Tab)} className="gap-0">
          <div className="mb-5 flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
            <TabsList className="glass-surface rounded-xl p-1">
              <TabsTab value="overview" className="px-3"><LayoutDashboard /> 概览</TabsTab>
              <TabsTab value="agents" className="px-3"><Bot /> Agent</TabsTab>
              <TabsTab value="logs" className="px-3"><ScrollText /> 请求日志</TabsTab>
            </TabsList>
            <div role="group" aria-label="按 Agent 筛选" className="flex flex-wrap items-center gap-1.5">
              <FilterChip active={agentId === null} onClick={() => setAgentId(null)} label="全部" count={totalRequests} />
              {agents.map((item) => (
                <FilterChip
                  key={item.agent.id}
                  active={agentId === item.agent.id}
                  onClick={() => setAgentId(agentId === item.agent.id ? null : item.agent.id)}
                  label={item.agent.name}
                  count={item.totals.requests}
                  dot={<AgentDot agent={item.agent} className="size-2" />}
                />
              ))}
            </div>
          </div>

          <TabsPanel value="overview">
            <OverviewView entries={entries} meta={meta} scope={scope} selectedDay={selectedDay} onSelectDay={setSelectedDay} onDrillDay={drillDay} />
          </TabsPanel>
          <TabsPanel value="agents">
            <AgentsView entries={entries} onDrill={drillAgent} />
          </TabsPanel>
          <TabsPanel value="logs">
            <LogsView
              key={agentId ?? 'all'}
              filters={logFilters}
              onFiltersChange={setLogFilters}
              userAgents={selectedUserAgents}
              meta={meta}
              version={`${authVersion}:${logsVersion}`}
              canClear={meta?.enabled === true}
              onRequestClear={() => setClearOpen(true)}
              onSelect={setSelectedLog}
            />
          </TabsPanel>
        </Tabs>
      </div>

      {apiKeyOpen && <Modal title="Gateway API Key" onClose={() => setAPIKeyOpen(false)}>
        <form className="space-y-5" onSubmit={saveAPIKey}>
          <p className="text-sm leading-6 text-muted-foreground">输入网关 Bearer Token 后才能读取请求日志。密钥仅保存在当前浏览器中。</p>
          <Input type="password" autoFocus value={apiKey} placeholder="sk-..." onChange={(event) => setAPIKey(event.target.value)} />
          <div className="flex justify-end gap-2"><Button variant="outline" onClick={() => setAPIKeyOpen(false)}>取消</Button><Button type="submit">保存</Button></div>
        </form>
      </Modal>}

      {clearOpen && <Modal title="清除所有请求日志？" onClose={() => !isClearing && setClearOpen(false)}>
        <p className="text-sm leading-6 text-muted-foreground">该操作会永久删除所有原始请求日志，无法撤销。按小时的用量汇总（热力图、Agent 统计）会保留。</p>
        <div className="mt-6 flex justify-end gap-2"><Button variant="outline" disabled={isClearing} onClick={() => setClearOpen(false)}>取消</Button><Button variant="destructive" loading={isClearing} onClick={() => void clearLogs()}>清除全部</Button></div>
      </Modal>}

      {selectedLog && <Modal title="请求详情" onClose={() => setSelectedLog(null)} wide>
        <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-[9rem_1fr]">
          <Detail label="request_id" value={selectedLog.request_id} />
          <Detail label="time" value={formatDateTime(selectedLog.timestamp)} />
          <Detail label="agent" value={selectedLog.agent.name} />
          <Detail label="outcome" value={outcomeLabel(selectedLog.outcome)} />
          <Detail label="status" value={`HTTP ${httpStatus(selectedLog.status_code)} · ${selectedLog.duration_ms} ms`} />
          <Detail label="usage" value={selectedLog.usage_complete ? '完整' : '未完整'} />
          <Detail label="retried" value={selectedLog.retried ? '是：这次尝试失败后，同一请求改用了其它上游' : '否：这是该请求的最终结果'} />
          <Detail label="input tokens" value={detailedTokens(selectedLog.input_tokens, selectedLog.usage_complete)} />
          <Detail label="output tokens" value={detailedTokens(selectedLog.output_tokens, selectedLog.usage_complete)} />
          <Detail label="cached tokens" value={detailedTokens(selectedLog.cached_tokens, selectedLog.usage_complete)} />
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

function FilterChip({ active, onClick, label, count, dot }: { active: boolean; onClick: () => void; label: string; count: number; dot?: React.ReactNode }) {
  return (
    <button
      type="button"
      aria-pressed={active}
      className={cn(
        'inline-flex h-8 cursor-pointer items-center gap-1.5 rounded-full border px-3 text-xs font-medium transition-colors outline-none focus-visible:ring-2 focus-visible:ring-ring',
        active ? 'border-foreground/20 bg-foreground text-background' : 'border-(--glass-divider) bg-(--glass-surface-strong) text-foreground hover:bg-background',
      )}
      onClick={onClick}
    >
      {dot}
      {label}
      <span className={cn('tabular-nums', active ? 'text-background/70' : 'text-muted-foreground')}>{formatInteger(count)}</span>
    </button>
  )
}

export default App
