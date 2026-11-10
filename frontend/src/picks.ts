// Values worth offering for a field, read off the events seen lately: who has
// been writing, and in which conversations.
import type { HubEvent } from './types'
import type { VarGroup } from './components/VarPicker.vue'
import type { Picks } from './components/SchemaForm.vue'

const kinds: Record<string, string> = { group: '群聊', private: '私聊', channel: '频道' }

export function recentActors(events: HubEvent[]): VarGroup[] {
  const seen = new Map<string, { expr: string; label: string; sample: string }>()
  for (const e of events) {
    if (!e.actor?.id || e.actor.is_bot || seen.has(e.actor.id)) continue
    seen.set(e.actor.id, { expr: e.actor.id, label: e.actor.name || e.actor.id, sample: e.actor.id })
  }
  return [{ title: '最近发过消息的人', items: [...seen.values()] }]
}

export function recentConversations(events: HubEvent[], source: string): VarGroup[] {
  const seen = new Map<string, { expr: string; label: string; sample: string }>()
  for (const e of events) {
    const c = e.conversation
    if (e.source !== source || !c?.id || seen.has(c.id)) continue
    const who = c.kind === 'private' ? e.actor?.name : ''
    seen.set(c.id, {
      expr: c.id,
      label: c.name || who || c.id,
      sample: [kinds[c.kind || ''] || c.kind, c.name || who ? c.id : ''].filter(Boolean).join(' · '),
    })
  }
  return [{ title: '最近有消息的会话', items: [...seen.values()] }]
}

export const picksFor = (events: HubEvent[], source = ''): Picks => ({
  actor: recentActors(events),
  conversation: source ? recentConversations(events, source) : undefined,
})
