import type React from 'react'

export type TooltipState = { x: number; y: number; content: React.ReactNode } | null

export function tooltipAt(element: Element, content: React.ReactNode): TooltipState {
  const rect = element.getBoundingClientRect()
  const x = Math.min(Math.max(rect.left + rect.width / 2, 96), window.innerWidth - 96)
  return { x, y: rect.top, content }
}
