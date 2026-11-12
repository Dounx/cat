import { defineStore } from 'pinia'
import { auth, get } from './api'
import type { HubEvent, Instance, Plugin, Rule, Run } from './types'

const MAX_EVENTS = 500
const MAX_RUNS = 300

export const useHub = defineStore('hub', {
  state: () => ({
    plugins: [] as Plugin[],
    instances: [] as Instance[],
    rules: [] as Rule[],
    events: [] as HubEvent[],
    runs: [] as Run[],
    connected: false,
    needAuth: false,
    loaded: false,
    toast: null as null | { text: string; bad: boolean },
    signals: {} as Record<string, unknown>,
  }),
  getters: {
    pluginOf: (s) => (id: string) => s.plugins.find((p) => p.id === id),
    instanceOf: (s) => (id: string) => s.instances.find((i) => i.id === id),
    instanceName: (s) => (id: string) => s.instances.find((i) => i.id === id)?.name || id,
    // Whether an instance is there without anyone having set it up: a
    // built-in action's, or the one a plugin that is told everything by the
    // rules comes with. Such an instance is not a connection to look after.
    given: (s) => (i: { id: string; plugin: string }) => {
      const p = s.plugins.find((p) => p.id === i.plugin)
      return !!p && (!!p.singleton || (!!p.default_instance && i.id === p.id))
    },
  },
  actions: {
    async init() {
      auth.onUnauthorized = () => {
        this.needAuth = true
      }
      await this.reload()
      this.listen()
    },
    async reload() {
      try {
        const [plugins, instances, rules, events, runs] = await Promise.all([
          get<Plugin[]>('/plugins'),
          get<Instance[]>('/instances'),
          get<Rule[]>('/rules'),
          get<HubEvent[]>('/events?limit=200'),
          get<Run[]>('/runs?limit=200'),
        ])
        Object.assign(this, { plugins, instances, rules, events, runs, loaded: true, needAuth: false })
      } catch {
        /* 401 is surfaced through needAuth */
      }
    },
    async reloadRules() {
      this.rules = await get<Rule[]>('/rules')
    },
    async reloadPlugins() {
      this.plugins = await get<Plugin[]>('/plugins')
    },
    async reloadInstances() {
      this.instances = await get<Instance[]>('/instances')
    },
    listen() {
      const es = new EventSource('/api/stream')
      es.onopen = () => {
        this.connected = true
      }
      es.onerror = () => {
        this.connected = false
      }
      es.addEventListener('event', (m) => {
        this.events.unshift(JSON.parse((m as MessageEvent).data))
        if (this.events.length > MAX_EVENTS) this.events.length = MAX_EVENTS
      })
      es.addEventListener('run', (m) => {
        const run: Run = JSON.parse((m as MessageEvent).data)
        const i = this.runs.findIndex((r) => r.id === run.id)
        if (i >= 0) this.runs[i] = run
        else this.runs.unshift(run)
        if (this.runs.length > MAX_RUNS) this.runs.length = MAX_RUNS
      })
      es.addEventListener('instance', (m) => {
        const inst: Instance = JSON.parse((m as MessageEvent).data)
        const i = this.instances.findIndex((x) => x.id === inst.id)
        if (i >= 0) this.instances[i] = inst
        else this.instances.push(inst)
      })
      // a rule has something new to show, e.g. a subscription its source cannot read
      es.addEventListener('rules', () => void this.reloadRules())
      // Other kinds (setup, download, ...) are exposed as the latest payload per kind.
      for (const kind of ['setup', 'download', 'plugins']) {
        es.addEventListener(kind, (m) => {
          this.signals[kind] = JSON.parse((m as MessageEvent).data)
        })
      }
    },
    notify(text: string, bad = false) {
      this.toast = { text, bad }
      setTimeout(
        () => {
          if (this.toast?.text === text) this.toast = null
        },
        bad ? 6000 : 2500,
      )
    },
    async run<T>(fn: () => Promise<T>, okText?: string): Promise<T | undefined> {
      try {
        const out = await fn()
        if (okText) this.notify(okText)
        return out
      } catch (e: any) {
        this.notify(e.message || String(e), true)
      }
    },
  },
})

export function timeAgo(iso: string): string {
  const d = new Date(iso)
  const s = (Date.now() - d.getTime()) / 1000
  if (s < 60) return '刚刚'
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`
  return d.toLocaleDateString()
}

export function clock(iso: string): string {
  const d = new Date(iso)
  const today = new Date().toDateString() === d.toDateString()
  const t = d.toLocaleTimeString('zh-CN', { hour12: false })
  return today ? t : `${d.getMonth() + 1}/${d.getDate()} ${t}`
}

export function summarize(ev: HubEvent): string {
  const p = ev.payload || {}
  if (typeof p.text === 'string' && p.text) return p.text
  const s = JSON.stringify(p)
  return s === '{}' ? '' : s
}
