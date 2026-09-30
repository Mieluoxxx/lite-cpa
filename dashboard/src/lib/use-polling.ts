import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from './api'

export type Polled<T> = {
  data: T | null
  error: Error | null
  loading: boolean
  syncedAt: number | null
  reload: () => void
}

// Polls path every intervalMs while the tab is visible. A null path pauses
// polling and keeps the last data. version forces a refetch (new API key,
// after "clear logs") without changing the path.
export function usePolling<T>(path: string | null, intervalMs: number, version: unknown = 0): Polled<T> {
  const [state, setState] = useState<Omit<Polled<T>, 'reload'>>({ data: null, error: null, loading: path !== null, syncedAt: null })
  const sequence = useRef(0)

  const load = useCallback(async () => {
    if (!path) return
    const id = ++sequence.current
    try {
      const data = await api<T>(path)
      // Drop responses that a newer request (e.g. a filter change) superseded.
      if (id === sequence.current) setState({ data, error: null, loading: false, syncedAt: Date.now() })
    } catch (error) {
      if (id === sequence.current) {
        setState((current) => ({ ...current, error: error instanceof Error ? error : new Error(String(error)), loading: false }))
      }
    }
  }, [path])

  useEffect(() => {
    if (!path) return
    void load()
    if (intervalMs <= 0) return
    const timer = window.setInterval(() => {
      if (!document.hidden) void load()
    }, intervalMs)
    return () => window.clearInterval(timer)
  }, [load, path, intervalMs, version])

  const reload = useCallback(() => void load(), [load])
  return { ...state, reload }
}
