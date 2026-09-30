import { X } from 'lucide-react'
import type React from 'react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'

export function Modal({ children, onClose, title, wide = false }: { children: React.ReactNode; onClose: () => void; title: string; wide?: boolean }) {
  return <div className="fixed inset-0 z-50 grid place-items-center bg-slate-950/42 p-4 backdrop-blur-md dark:bg-black/58" role="presentation" onMouseDown={onClose}>
    <Card className={`glass-modal max-h-[calc(100vh-2rem)] w-full overflow-auto rounded-[1.5rem] ${wide ? 'max-w-3xl' : 'max-w-md'}`} role="dialog" aria-modal="true" aria-label={title} onMouseDown={(event) => event.stopPropagation()}>
      <CardContent className="p-5 sm:p-6"><div className="mb-5 flex items-center justify-between gap-4"><h2 className="text-lg font-semibold tracking-tight">{title}</h2><Button variant="ghost" size="icon-sm" aria-label="关闭" onClick={onClose}><X /></Button></div>{children}</CardContent>
    </Card>
  </div>
}

export function Detail({ label, value }: { label: string; value: string }) {
  return <><dt className="font-mono text-xs text-muted-foreground">{label}</dt><dd className="break-words font-mono text-xs leading-5 text-foreground">{value}</dd></>
}

export function Payload({ title, value }: { title: string; value: string }) {
  return <section><h3 className="mb-2 text-sm font-medium">{title}</h3><pre className="max-h-64 overflow-auto rounded-xl bg-muted p-4 text-xs leading-5 whitespace-pre-wrap break-words">{value}</pre></section>
}
