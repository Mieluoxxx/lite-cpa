import { createPortal } from 'react-dom'
import { Card, CardContent } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import type { Agent } from '../lib/agents'
import { compact, formatRate } from '../lib/format'
import type { TooltipState } from '../lib/tooltip'
import type { Totals } from '../lib/usage'

export function MetricCard({ label, value, hint, tone }: { label: string; value: string; hint: string; tone?: string }) {
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

export function Segmented<T extends string>({ label, value, options, onChange }: {
  label: string
  value: T
  options: { value: T; label: string }[]
  onChange: (value: T) => void
}) {
  return (
    <div role="group" aria-label={label} className="inline-flex shrink-0 rounded-lg bg-muted p-0.5">
      {options.map((option) => {
        const active = option.value === value
        return (
          <button
            key={option.value}
            type="button"
            aria-pressed={active}
            className={cn(
              'h-7 cursor-pointer rounded-md px-2.5 text-xs font-medium transition-colors outline-none focus-visible:ring-2 focus-visible:ring-ring',
              active ? 'bg-background text-foreground shadow-xs/5 dark:bg-input' : 'text-muted-foreground hover:text-foreground',
            )}
            onClick={() => onChange(option.value)}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}

export function AgentDot({ agent, className }: { agent: Agent; className?: string }) {
  return <span aria-hidden className={cn('inline-block size-2.5 shrink-0 rounded-full', className)} style={{ background: agent.color }} />
}

// Portaled because the glass cards use backdrop-filter, which would otherwise
// become the containing block for a position:fixed tooltip.
export function ChartTooltip({ state }: { state: TooltipState }) {
  if (!state) return null
  return createPortal(
    <div
      role="tooltip"
      className="pointer-events-none fixed z-50 max-w-72 rounded-xl border bg-popover px-3 py-2 text-xs text-popover-foreground shadow-lg"
      style={{ left: state.x, top: state.y, transform: 'translate(-50%, calc(-100% - 8px))' }}
    >
      {state.content}
    </div>,
    document.body,
  )
}

export function TooltipSeriesRow({ color, label, value }: { color: string; label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <span aria-hidden className="h-0.5 w-3 shrink-0 rounded-full" style={{ background: color }} />
      <span className="font-semibold tabular-nums text-foreground">{value}</span>
      <span className="truncate text-muted-foreground">{label}</span>
    </div>
  )
}

const MIX_SEGMENTS = [
  { key: 'cached', label: '缓存输入', color: 'var(--mix-cached)' },
  { key: 'input', label: '未缓存输入', color: 'var(--mix-input)' },
  { key: 'output', label: '输出', color: 'var(--mix-output)' },
] as const

function mixParts(totals: Totals) {
  const cached = Math.min(totals.cached, totals.input)
  const parts = { cached, input: Math.max(0, totals.input - cached), output: totals.output }
  return { parts, sum: parts.cached + parts.input + parts.output }
}

export function TokenMixBar({ totals, className }: { totals: Totals; className?: string }) {
  const { parts, sum } = mixParts(totals)
  if (sum <= 0) return <div className={cn('h-2 rounded-full bg-muted', className)} />
  const label = MIX_SEGMENTS.map((segment) => `${segment.label} ${formatRate(parts[segment.key] / sum)}`).join('，')
  return (
    <div role="img" aria-label={label} title={label} className={cn('flex h-2 gap-[2px] overflow-hidden rounded-full', className)}>
      {MIX_SEGMENTS.map((segment) => parts[segment.key] > 0 && (
        <span key={segment.key} className="min-w-[2px] basis-0" style={{ flexGrow: parts[segment.key], background: segment.color }} />
      ))}
    </div>
  )
}

export function TokenMixLegend({ totals }: { totals: Totals }) {
  const { parts, sum } = mixParts(totals)
  return (
    <dl className="grid grid-cols-3 gap-2">
      {MIX_SEGMENTS.map((segment) => (
        <div key={segment.key} className="min-w-0">
          <dt className="flex items-center gap-1.5 text-[0.7rem] text-muted-foreground">
            <span aria-hidden className="size-2 shrink-0 rounded-[2px]" style={{ background: segment.color }} />
            <span className="truncate">{segment.label}</span>
          </dt>
          <dd className="mt-0.5 text-sm font-semibold text-foreground">
            {compact(parts[segment.key])}
            <span className="ml-1 text-[0.7rem] font-normal text-muted-foreground">{sum > 0 ? formatRate(parts[segment.key] / sum) : '—'}</span>
          </dd>
        </div>
      ))}
    </dl>
  )
}
