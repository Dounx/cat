import { describe, expect, it } from 'vitest'
import { compile, compileRow, operators } from './condition'

const row = (op: string, value = '', field = 'payload.text') => compileRow({ field, op, value })

describe('compileRow', () => {
  it('reads a missing parent as nothing rather than an error', () => {
    expect(row('exists', '', 'payload.user.id')).toBe('payload?.user?.id != nil')
    expect(row('not_exists')).toBe('payload?.text == nil')
  })

  it('compares text against an empty string when the field is missing', () => {
    expect(row('contains', 'deploy')).toBe('(payload?.text ?? "") contains "deploy"')
    expect(row('not_contains', 'deploy')).toBe('not ((payload?.text ?? "") contains "deploy")')
    expect(row('starts', '/')).toBe('(payload?.text ?? "") startsWith "/"')
    expect(row('ends', '?')).toBe('(payload?.text ?? "") endsWith "?"')
    expect(row('matches', '^\\d+$')).toBe('(payload?.text ?? "") matches "^\\\\d+$"')
  })

  it('quotes what is typed, quotes included', () => {
    expect(row('eq', 'say "hi"')).toBe('payload?.text == "say \\"hi\\""')
  })

  it('matches a number typed bare against both the number and the string', () => {
    expect(row('eq', '123', 'actor.id')).toBe('(actor?.id == 123 or actor?.id == "123")')
    expect(row('ne', '123', 'actor.id')).toBe('not ((actor?.id == 123 or actor?.id == "123"))')
  })

  it('takes true and false as booleans', () => {
    expect(row('eq', 'true', 'payload.mentioned')).toBe('payload?.mentioned == true')
    expect(row('eq', ' false ', 'payload.mentioned')).toBe('payload?.mentioned == false')
  })

  it('splits a list on either kind of comma and drops blanks', () => {
    expect(row('in', 'a, b，,c', 'type')).toBe('(type == "a" or type == "b" or type == "c")')
  })

  it('compares numbers, taking anything else as zero', () => {
    expect(row('gt', '5', 'payload.count')).toBe('payload?.count > 5')
    expect(row('lt', 'many', 'payload.count')).toBe('payload?.count < 0')
  })

  it('lets an operator it does not know through', () => {
    expect(row('nonsense', 'x')).toBe('true')
  })

  it('knows every operator the builder offers', () => {
    for (const o of operators) expect(row(o.id, '1'), o.id).not.toBe('true')
  })
})

describe('compile', () => {
  const rows = [
    { field: 'payload.text', op: 'contains', value: 'a' },
    { field: '  ', op: 'eq', value: 'ignored' },
    { field: 'type', op: 'eq', value: 'message.created' },
  ]

  it('joins the rows that name a field', () => {
    expect(compile({ mode: 'builder', join: 'and', rows })).toBe(
      '(payload?.text ?? "") contains "a" and type == "message.created"',
    )
    expect(compile({ mode: 'builder', join: 'or', rows })).toContain('" or type ==')
  })

  it('is empty, which always holds, with no rows', () => {
    expect(compile({ mode: 'builder', join: 'and', rows: [] })).toBe('')
  })
})
