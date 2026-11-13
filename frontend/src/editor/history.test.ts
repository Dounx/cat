import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { effectScope, nextTick, reactive } from 'vue'
import { useHistory } from './history'

describe('useHistory', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  function open() {
    const doc = reactive({ name: 'a', nodes: [] as string[] })
    const restored = vi.fn()
    const scope = effectScope()
    const history = scope.run(() => useHistory(doc, restored))!
    // a change as the editor sees it: made, noticed, and left to settle
    const change = async (fn: () => void) => {
      fn()
      await nextTick()
      vi.advanceTimersByTime(300)
    }
    return { doc, restored, scope, change, ...history }
  }

  it('starts with nothing to undo and nothing unsaved', () => {
    const { state, settle } = open()
    settle()
    expect(state).toEqual({ undo: false, redo: false, dirty: false })
  })

  it('takes a run of quick changes as one step', async () => {
    const { doc, state, undo } = open()
    doc.name = 'ab'
    await nextTick()
    vi.advanceTimersByTime(100)
    doc.name = 'abc'
    await nextTick()
    expect(state.undo).toBe(false) // not settled yet
    vi.advanceTimersByTime(300)
    expect(state).toEqual({ undo: true, redo: false, dirty: true })

    undo()
    expect(doc.name).toBe('a')
  })

  it('goes back and forth, and is clean again where it started', async () => {
    const { doc, state, change, undo, redo, restored } = open()
    await change(() => (doc.name = 'b'))
    await change(() => doc.nodes.push('t1'))

    undo()
    await nextTick()
    expect(doc).toEqual({ name: 'b', nodes: [] })
    expect(state).toEqual({ undo: true, redo: true, dirty: true })

    undo()
    await nextTick()
    expect(doc).toEqual({ name: 'a', nodes: [] })
    expect(state).toEqual({ undo: false, redo: true, dirty: false })

    redo()
    redo()
    await nextTick()
    expect(doc).toEqual({ name: 'b', nodes: ['t1'] })
    expect(state).toEqual({ undo: true, redo: false, dirty: true })
    expect(restored).toHaveBeenCalledTimes(4)
  })

  it('does not take being put back for a change of its own', async () => {
    const { doc, state, change, undo } = open()
    await change(() => (doc.name = 'b'))
    undo()
    await nextTick()
    vi.advanceTimersByTime(300)
    expect(state).toEqual({ undo: false, redo: true, dirty: false })
  })

  it('forgets what was undone once something else is done', async () => {
    const { doc, state, change, undo, redo } = open()
    await change(() => (doc.name = 'b'))
    undo()
    await nextTick()
    await change(() => (doc.name = 'c'))
    expect(state.redo).toBe(false)
    redo()
    expect(doc.name).toBe('c')
  })

  it('undoes a change that has not settled yet', async () => {
    const { doc, undo } = open()
    doc.name = 'b'
    await nextTick()
    undo()
    expect(doc.name).toBe('a')
  })

  it('stops waiting when its scope ends', async () => {
    const { doc, state, scope } = open()
    doc.name = 'b'
    await nextTick()
    scope.stop()
    vi.advanceTimersByTime(300)
    expect(state.undo).toBe(false)
  })
})
