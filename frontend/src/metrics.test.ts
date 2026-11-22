import { describe, expect, it } from 'vitest'
import { formatTokens, relativeTime, stepTokens, summarizeRules, tokenBreakdown } from './metrics'
import type { Rule, StepResult } from './types'

describe('metrics helpers', () => {
  it('formats token counts compactly', () => {
    expect(formatTokens(0)).toBe('0')
    expect(formatTokens(605)).toBe('605')
    expect(formatTokens(1000)).toBe('1k')
    expect(formatTokens(1420)).toBe('1.4k')
    expect(formatTokens(1815)).toBe('1.8k')
    expect(formatTokens(12540)).toBe('12.5k')
    expect(formatTokens(250_400)).toBe('250k')
    expect(formatTokens(1_500_000)).toBe('1.5M')
  })

  it('builds readable token breakdown tooltips', () => {
    expect(tokenBreakdown({ prompt: 1260, completion: 555, total: 1815 })).toBe(
      '近 24 小时共消耗 1,815 tokens（输入 1,260 · 输出 555）',
    )
    expect(tokenBreakdown({ prompt: 0, completion: 0, total: 900 }, '本步')).toBe('本步共消耗 900 tokens')
  })

  it('formats relative times accurately', () => {
    const now = Date.UTC(2026, 10, 22, 12, 0, 0)
    expect(relativeTime(new Date(now - 20_000).toISOString(), now)).toBe('刚刚')
    expect(relativeTime(new Date(now - 5 * 60_000).toISOString(), now)).toBe('5 分钟前')
    expect(relativeTime(new Date(now - 3 * 3600_000).toISOString(), now)).toBe('3 小时前')
    expect(relativeTime(new Date(now - 2 * 86400_000).toISOString(), now)).toBe('2 天前')
  })

  it('summarizes rule stats across rules', () => {
    const rules: Rule[] = [
      {
        id: 'r1',
        name: 'A',
        enabled: true,
        steps: [],
        stats: {
          rule_id: 'r1',
          runs_24h: 3,
          ok_24h: 3,
          failed_24h: 0,
          running: 0,
          tokens_24h: { prompt: 1260, completion: 555, total: 1815 },
          total_runs: 3,
          total_tokens: 1815,
        },
      },
      {
        id: 'r2',
        name: 'B',
        enabled: true,
        steps: [],
        stats: {
          rule_id: 'r2',
          runs_24h: 2,
          ok_24h: 0,
          failed_24h: 2,
          running: 1,
          tokens_24h: { prompt: 0, completion: 0, total: 0 },
          total_runs: 2,
          total_tokens: 0,
        },
      },
      { id: 'r3', name: 'Idle', enabled: true, steps: [] },
    ]
    expect(summarizeRules(rules)).toEqual({
      runs24h: 5,
      ok24h: 3,
      failed24h: 2,
      running: 1,
      tokens24h: { prompt: 1260, completion: 555, total: 1815 },
    })
  })

  it('extracts OpenAI and Anthropic token usage from step outputs', () => {
    const openai: StepResult = {
      id: 's1',
      instance: 'llm',
      action: 'chat',
      duration_ms: 120,
      output: { usage: { prompt_tokens: 420, completion_tokens: 185 } },
    }
    expect(stepTokens(openai)).toEqual({ prompt: 420, completion: 185, total: 605 })

    const anthropic: StepResult = {
      id: 's2',
      instance: 'claude',
      action: 'chat',
      duration_ms: 200,
      output: { usage: { input_tokens: 300, output_tokens: 90 } },
    }
    expect(stepTokens(anthropic)).toEqual({ prompt: 300, completion: 90, total: 390 })

    const none: StepResult = { id: 's3', instance: 'http', action: 'request', duration_ms: 50, output: { status: 200 } }
    expect(stepTokens(none)).toBeNull()
  })
})
