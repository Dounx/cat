// The visual condition builder: rows of (field, operator, value) joined by
// and/or, compiled to an expr-lang expression the engine evaluates.
export interface Row { field: string; op: string; value: string }
export interface Builder { mode: 'builder' | 'expr'; join: 'and' | 'or'; rows: Row[] }

export const operators: { id: string; label: string; noValue?: boolean }[] = [
  { id: 'contains', label: '包含' },
  { id: 'not_contains', label: '不包含' },
  { id: 'eq', label: '等于' },
  { id: 'ne', label: '不等于' },
  { id: 'starts', label: '开头是' },
  { id: 'ends', label: '结尾是' },
  { id: 'matches', label: '匹配正则' },
  { id: 'in', label: '是其中之一' },
  { id: 'gt', label: '大于' },
  { id: 'lt', label: '小于' },
  { id: 'exists', label: '存在', noValue: true },
  { id: 'not_exists', label: '不存在', noValue: true },
]

export const commonFields = ['payload.text', 'actor.id', 'actor.name', 'conversation.id', 'conversation.kind', 'type', 'source']

// a.b.c -> a?.b?.c so a missing parent yields nil instead of an error.
const safe = (field: string) => field.trim().replace(/\./g, '?.')
const q = (v: string) => JSON.stringify(v)
const isNum = (v: string) => v.trim() !== '' && !isNaN(Number(v))
// IDs arrive as strings but users type them bare, so a numeric value matches both forms.
const same = (f: string, v: string) => (isNum(v) ? `(${f} == ${Number(v)} or ${f} == ${q(v.trim())})` : `${f} == ${q(v)}`)

export function compileRow(r: Row): string {
  const f = safe(r.field)
  const text = `(${f} ?? "")`
  switch (r.op) {
    case 'contains': return `${text} contains ${q(r.value)}`
    case 'not_contains': return `not (${text} contains ${q(r.value)})`
    case 'eq': return same(f, r.value)
    case 'ne': return `not ${isNum(r.value) ? same(f, r.value) : `(${same(f, r.value)})`}`
    case 'starts': return `${text} startsWith ${q(r.value)}`
    case 'ends': return `${text} endsWith ${q(r.value)}`
    case 'matches': return `${text} matches ${q(r.value)}`
    case 'in': return '(' + r.value.split(/[,，]/).map((v) => v.trim()).filter(Boolean).map((v) => same(f, v)).join(' or ') + ')'
    case 'gt': return `${f} > ${Number(r.value) || 0}`
    case 'lt': return `${f} < ${Number(r.value) || 0}`
    case 'exists': return `${f} != nil`
    case 'not_exists': return `${f} == nil`
  }
  return 'true'
}

export function compile(b: Builder): string {
  const rows = b.rows.filter((r) => r.field.trim())
  return rows.map(compileRow).join(` ${b.join} `)
}
