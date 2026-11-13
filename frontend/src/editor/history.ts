// Undo for a document held as one plain reactive object: every settled
// change to it is kept as a snapshot.
import { nextTick, onScopeDispose, reactive, watch } from 'vue'

const KEPT = 200
// A run of changes closer together than this, typing say, is one step.
const SETTLE_MS = 250

// `restored` is called after the document has been put back to a snapshot,
// for whatever points into it (a selection, say) to catch up.
export function useHistory(doc: object, restored: () => void = () => {}) {
  let last = JSON.stringify(doc)
  const saved = last
  const past: string[] = []
  const future: string[] = []
  const state = reactive({ undo: false, redo: false, dirty: false })
  let restoring = false
  let timer: ReturnType<typeof setTimeout> | undefined

  // Take the document as it is now for a step, without waiting for it to
  // settle by itself.
  function settle() {
    clearTimeout(timer)
    const now = JSON.stringify(doc)
    if (now !== last) {
      past.push(last)
      if (past.length > KEPT) past.shift()
      future.length = 0
      last = now
    }
    Object.assign(state, { undo: past.length > 0, redo: future.length > 0, dirty: last !== saved })
  }
  watch(
    doc,
    () => {
      if (restoring) return
      clearTimeout(timer)
      timer = setTimeout(settle, SETTLE_MS)
    },
    { deep: true },
  )
  function restore(snapshot: string) {
    restoring = true
    Object.assign(doc, JSON.parse(snapshot))
    last = snapshot
    restored()
    nextTick(() => {
      restoring = false
      settle()
    })
  }
  function undo() {
    settle()
    if (!past.length) return
    future.push(last)
    restore(past.pop()!)
  }
  function redo() {
    settle()
    if (!future.length) return
    past.push(last)
    restore(future.pop()!)
  }
  onScopeDispose(() => clearTimeout(timer))

  return { state, settle, undo, redo }
}
