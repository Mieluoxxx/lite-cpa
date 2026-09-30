export type Agent = {
  id: string
  name: string
  color: string
  order: number
}

type Rule = {
  id: string
  name: string
  slot: number
  pattern: RegExp
}

// Slot N maps to --agent-N in index.css. Neighbouring slots were validated for
// colorblind separation, so append new agents instead of reshuffling slots.
const RULES: Rule[] = [
  { id: 'codex', name: 'Codex', slot: 1, pattern: /^codex[_-]/i },
  { id: 'claude-code', name: 'Claude Code', slot: 2, pattern: /^claude-cli\//i },
  { id: 'pi', name: 'Pi', slot: 3, pattern: /^pi[\s/(]/i },
  { id: 'opencode', name: 'OpenCode', slot: 4, pattern: /^opencode\//i },
  { id: 'kimi-code', name: 'Kimi Code', slot: 5, pattern: /^kimi/i },
  { id: 'mimo-code', name: 'MiMo Code', slot: 6, pattern: /^mimocode\//i },
  { id: 'oh-my-pi', name: 'Oh My Pi', slot: 7, pattern: /^(omp|oh-my-pi)\b/i },
]

const KNOWN = RULES.map((rule): [RegExp, Agent] => [
  rule.pattern,
  { id: rule.id, name: rule.name, color: `var(--agent-${rule.slot})`, order: rule.slot },
])

const OTHER_COLOR = 'var(--agent-other)'
const OTHER_ORDER = RULES.length + 1

export const UNKNOWN_AGENT: Agent = { id: 'unknown', name: '未知客户端', color: OTHER_COLOR, order: OTHER_ORDER + 1 }

const cache = new Map<string, Agent>()

export function agentFromUA(userAgent: string): Agent {
  const hit = cache.get(userAgent)
  if (hit) return hit
  const agent = detect(userAgent.trim())
  cache.set(userAgent, agent)
  return agent
}

function detect(userAgent: string): Agent {
  if (!userAgent) return UNKNOWN_AGENT
  for (const [pattern, agent] of KNOWN) {
    if (pattern.test(userAgent)) return agent
  }
  // Unrecognised clients still get their own group so new agents show up
  // without a code change; only the palette slot is reserved for known ones.
  const product = userAgent.split(/[\s/(;]/, 1)[0] || userAgent
  return { id: `ua:${product.toLowerCase()}`, name: product, color: OTHER_COLOR, order: OTHER_ORDER }
}

export function compareAgents(a: Agent, b: Agent): number {
  return a.order - b.order || a.name.localeCompare(b.name)
}
