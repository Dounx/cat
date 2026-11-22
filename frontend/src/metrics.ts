import type { Rule, StepResult, TokenUsage } from './types'

export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0'
  if (n < 1000) return String(Math.round(n))
  if (n < 100_000) return `${(n / 1000).toFixed(1).replace(/\.0$/, '')}k`
  if (n < 1_000_000) return `${Math.round(n / 1000)}k`
  return `${(n / 1_000_000).toFixed(1).replace(/\.0$/, '')}M`
}

export function tokenBreakdown(u: TokenUsage, prefix = '近 24 小时'): string {
  const total = u.total.toLocaleString('en-US')
  if (u.prompt > 0 || u.completion > 0) {
    return `${prefix}共消耗 ${total} tokens（输入 ${u.prompt.toLocaleString('en-US')} · 输出 ${u.completion.toLocaleString('en-US')}）`
  }
  return `${prefix}共消耗 ${total} tokens`
}

export function relativeTime(iso: string, now = Date.now()): string {
  const ts = new Date(iso).getTime()
  if (!Number.isFinite(ts)) return ''
  const s = Math.max(0, (now - ts) / 1000)
  if (s < 60) return '刚刚'
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`
  if (s < 86400 * 7) return `${Math.floor(s / 86400)} 天前`
  return new Date(ts).toLocaleDateString('zh-CN')
}

export interface RulesSummary {
  runs24h: number
  ok24h: number
  failed24h: number
  running: number
  tokens24h: TokenUsage
}

export function summarizeRules(rules: Rule[]): RulesSummary {
  const out: RulesSummary = {
    runs24h: 0,
    ok24h: 0,
    failed24h: 0,
    running: 0,
    tokens24h: { prompt: 0, completion: 0, total: 0 },
  }
  for (const r of rules) {
    const st = r.stats
    if (!st) continue
    out.runs24h += st.runs_24h || 0
    out.ok24h += st.ok_24h || 0
    out.failed24h += st.failed_24h || 0
    out.running += st.running || 0
    if (st.tokens_24h) {
      out.tokens24h.prompt += st.tokens_24h.prompt || 0
      out.tokens24h.completion += st.tokens_24h.completion || 0
      out.tokens24h.total += st.tokens_24h.total || 0
    }
  }
  return out
}

function numField(obj: Record<string, unknown>, key: string): number {
  const v = obj[key]
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? Math.round(v) : 0
}

export function stepTokens(s: StepResult): TokenUsage | null {
  const raw = s.output?.usage
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null
  const m = raw as Record<string, unknown>
  const prompt = numField(m, 'prompt_tokens') || numField(m, 'input_tokens')
  const completion = numField(m, 'completion_tokens') || numField(m, 'output_tokens')
  const total = numField(m, 'total_tokens') || prompt + completion
  if (total <= 0) return null
  return { prompt, completion, total }
}
