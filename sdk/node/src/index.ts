// SDK for writing Cat plugins in TypeScript / JavaScript.
//
// A plugin is an executable Cat starts once per configured instance and talks
// to over stdin/stdout (JSON-RPC 2.0, one message per line). Describe it in
// plugin.json, then call run() with your handlers. Never write to stdout
// yourself: it carries the protocol. Use host.log() or stderr.
import { createInterface } from 'node:readline'

export const PROTOCOL_VERSION = 1

export interface Conversation {
  id: string
  kind?: 'private' | 'group' | 'channel'
  name?: string
}
export interface Actor {
  id: string
  name?: string
  is_bot?: boolean
}

/**
 * What a plugin emits. For chat messages use type "message.created" with the
 * text in payload.text and the platform message id in payload.message_id, so
 * rules work the same across platforms.
 */
export interface CatEvent {
  id?: string
  source?: string
  plugin?: string
  type: string
  time?: string
  conversation?: Conversation
  actor?: Actor
  payload?: Record<string, unknown>
  raw?: unknown
  /**
   * The ids of the triggers this event is emitted for (see Trigger). Leave it
   * out for an event no rule asked for in particular.
   */
  triggers?: string[]
}

/**
 * Something a rule asks this instance to watch: the parameters filled in on
 * the rule's trigger node, shaped by plugin.json's trigger_params. Put its id
 * in CatEvent.triggers on the events emitted for it; the rule that asked takes
 * only those. The same parameters always come under the same id.
 */
export interface Trigger {
  id: string
  params: Record<string, any>
}

export interface ActionContext {
  event?: CatEvent
  rule_id?: string
  run_id?: string
}

export interface IngressRequest {
  method: string
  path: string
  query?: Record<string, string[]>
  headers?: Record<string, string>
  body: Uint8Array
}
export interface IngressResponse {
  status: number
  headers?: Record<string, string>
  body?: Uint8Array | string
}

export type StepKind = 'info' | 'progress' | 'qrcode' | 'link' | 'form' | 'confirm'
/** One screen of the onboarding wizard, rendered by Cat's own UI. */
export interface Step {
  kind: StepKind
  title: string
  description?: string
  /** For qrcode: the text to encode. */
  text?: string
  /** A data: URL shown as an image (e.g. a QR code the platform rendered). */
  image?: string
  url?: string
  /** 0..1, or negative for indeterminate. */
  progress?: number
  /** JSON Schema of the form. */
  schema?: Record<string, unknown>
  values?: Record<string, unknown>
  danger?: boolean
}

/** Thrown by setup helpers when the user closes the wizard. */
export class CancelledError extends Error {
  constructor() {
    super('cancelled by user')
  }
}

export class RpcError extends Error {
  constructor(
    public code: number,
    message: string,
    public data?: unknown,
  ) {
    super(message)
  }
}

const CODE_METHOD_NOT_FOUND = -32601
const CODE_INTERNAL = -32603
const CODE_CANCELLED = -32800

type Handler = (params: any, signal: AbortSignal) => unknown | Promise<unknown>

class Conn {
  private handlers = new Map<string, Handler>()
  private pending = new Map<number, { resolve: (v: any) => void; reject: (e: Error) => void }>()
  private inflight = new Map<string, AbortController>()
  private nextId = 0
  closed = false
  onClose = () => {}

  constructor(
    input: NodeJS.ReadableStream,
    private output: NodeJS.WritableStream,
  ) {
    const rl = createInterface({ input, crlfDelay: Infinity })
    rl.on('line', (line) => {
      if (line.trim()) this.dispatch(line)
    })
    rl.on('close', () => {
      this.closed = true
      for (const c of this.inflight.values()) c.abort()
      for (const p of this.pending.values()) p.reject(new Error('connection closed'))
      this.onClose()
    })
  }

  handle(method: string, h: Handler) {
    this.handlers.set(method, h)
  }

  private write(msg: Record<string, unknown>) {
    if (!this.closed) this.output.write(JSON.stringify({ jsonrpc: '2.0', ...msg }) + '\n')
  }

  private async dispatch(line: string) {
    let m: any
    try {
      m = JSON.parse(line)
    } catch {
      return
    }
    if (!m.method) {
      const p = this.pending.get(m.id)
      if (!p) return
      this.pending.delete(m.id)
      if (m.error) p.reject(new RpcError(m.error.code, m.error.message, m.error.data))
      else p.resolve(m.result)
      return
    }
    if (m.method === '$/cancel') {
      this.inflight.get(JSON.stringify(m.params?.id))?.abort()
      return
    }
    const h = this.handlers.get(m.method)
    const isRequest = m.id !== undefined && m.id !== null
    if (!h) {
      if (isRequest)
        this.write({ id: m.id, error: { code: CODE_METHOD_NOT_FOUND, message: 'method not found: ' + m.method } })
      return
    }
    const key = JSON.stringify(m.id)
    const ctl = new AbortController()
    if (isRequest) this.inflight.set(key, ctl)
    try {
      const result = await h(m.params ?? {}, ctl.signal)
      if (isRequest) this.write({ id: m.id, result: result ?? null })
    } catch (e: any) {
      if (!isRequest) return
      const error =
        e instanceof RpcError
          ? { code: e.code, message: e.message, data: e.data }
          : { code: CODE_INTERNAL, message: e?.message ?? String(e), data: e?.output }
      this.write({ id: m.id, error })
    } finally {
      this.inflight.delete(key)
    }
  }

  call<T = any>(method: string, params?: unknown, signal?: AbortSignal): Promise<T> {
    if (this.closed) return Promise.reject(new Error('connection closed'))
    const id = ++this.nextId
    return new Promise<T>((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      signal?.addEventListener(
        'abort',
        () => {
          if (!this.pending.delete(id)) return
          this.notify('$/cancel', { id })
          reject(new Error('aborted'))
        },
        { once: true },
      )
      this.write({ id, method, params })
    })
  }

  notify(method: string, params?: unknown) {
    this.write({ method, params })
  }
}

/** The plugin's handle on Cat. */
export class Host {
  /** Instance configuration, shaped by plugin.json's config_schema. */
  config: Record<string, any> = {}
  instanceId = ''
  /** A private, persistent directory for this instance. */
  dataDir = ''
  /**
   * What the enabled rules ask this instance to watch, as of the latest
   * update. Already filled in when start() runs.
   */
  triggers: Trigger[] = []

  /** @internal */
  constructor(private conn: Conn) {}

  /** Publishes an event; resolves to the id Cat assigned. */
  async emit(event: CatEvent): Promise<string> {
    return (await this.conn.call<{ id: string }>('event.emit', event)).id
  }
  log(level: 'debug' | 'info' | 'warn' | 'error', message: string) {
    this.conn.notify('log', { level, message })
  }
  /** Reports connection health after start() has resolved. */
  setStatus(status: 'running' | 'error', message = '') {
    this.conn.notify('status', { status, message })
  }
  /** Reads a value persisted with stateSet (survives restarts). */
  async stateGet(key: string): Promise<string | undefined> {
    const out = await this.conn.call<{ value: string; found: boolean }>('state.get', { key })
    return out.found ? out.value : undefined
  }
  async stateSet(key: string, value: string): Promise<void> {
    await this.conn.call('state.set', { key, value })
  }
  /**
   * Asks Cat to make a dependency declared in plugin.json available
   * (downloading and verifying it if needed); resolves to its directory.
   */
  async dependency(name: string, signal?: AbortSignal): Promise<string> {
    return (await this.conn.call<{ path: string }>('host.dependency', { name }, signal)).path
  }
}

/** Drives the onboarding wizard. */
export class Setup {
  /** @internal */
  constructor(
    readonly host: Host,
    private conn: Conn,
    readonly existing: Record<string, any>,
    private signal: AbortSignal,
  ) {}

  /** Displays a step and returns immediately. */
  async show(step: Step): Promise<void> {
    await this.conn.call('setup.show', step)
  }
  /** Displays a form or confirm step and waits for the user's answer. */
  async ask(step: Step): Promise<Record<string, any>> {
    try {
      return await this.conn.call('setup.ask', step, this.signal)
    } catch (e) {
      if (e instanceof RpcError && e.code === CODE_CANCELLED) throw new CancelledError()
      throw e
    }
  }
  progress(title: string, fraction = -1) {
    return this.show({ kind: 'progress', title, progress: fraction })
  }
  async confirm(title: string, description?: string): Promise<boolean> {
    return (await this.ask({ kind: 'confirm', title, description })).confirmed === true
  }
}

/** Throw from an action to fail the step while still reporting partial output. */
export class ActionError extends Error {
  constructor(
    message: string,
    public output?: Record<string, unknown>,
  ) {
    super(message)
  }
}

export type Action = (
  params: Record<string, any>,
  ctx: ActionContext,
  host: Host,
  signal: AbortSignal,
) => unknown | Promise<unknown>

/** What a plugin implements. Every member is optional. */
export interface Plugin {
  /**
   * Connect to the platform and begin emitting events. Resolve once ready (or
   * reject on failure). `signal` aborts when the instance stops.
   */
  start?(host: Host, signal: AbortSignal): void | Promise<void>
  stop?(): void | Promise<void>
  /** Actions by name, as declared in plugin.json. Return an object. */
  actions?: Record<string, Action>
  /** HTTP requests for plugins with "ingress": true. */
  ingress?(req: IngressRequest, host: Host): IngressResponse | Promise<IngressResponse>
  /** Runs the onboarding wizard; resolves to the instance configuration. */
  setup?(setup: Setup, signal: AbortSignal): Promise<Record<string, unknown>>
  /**
   * Lists what may go in a field marked "x-pick": "option" while someone fills
   * in a form: a parameter of the named action, or a configuration field when
   * `action` is "". Runs on a started instance.
   */
  options?(action: string, field: string, host: Host, signal: AbortSignal): Option[] | Promise<Option[]>
  /**
   * Told what the enabled rules ask this instance to watch, for plugins whose
   * plugin.json has trigger_params: the whole list, each time it changes. The
   * first call comes before start(), which can read host.triggers instead.
   * Return what is wrong with single triggers, by id; it is shown on the
   * rules that asked for them.
   */
  triggers?(list: Trigger[], host: Host): void | Record<string, string> | Promise<void | Record<string, string>>
}

/** One value offered for a field. `title` is what people read, `note` a few words more. */
export interface Option {
  value: string
  title?: string
  note?: string
}

/** Serves the plugin over stdin/stdout until Cat closes the connection. */
export function run(
  plugin: Plugin,
  io: { input: NodeJS.ReadableStream; output: NodeJS.WritableStream } = {
    input: process.stdin,
    output: process.stdout,
  },
) {
  const conn = new Conn(io.input, io.output)
  const host = new Host(conn)
  const life = new AbortController()
  let stopped = false
  const stop = async () => {
    if (stopped) return
    stopped = true
    life.abort()
    await plugin.stop?.()
  }

  conn.handle('initialize', (p) => {
    host.config = p.config ?? {}
    host.instanceId = p.instance?.id ?? ''
    host.dataDir = p.data_dir ?? ''
    return { protocol: PROTOCOL_VERSION }
  })
  conn.handle('start', () => plugin.start?.(host, life.signal))
  conn.handle('stop', stop)
  conn.handle('action.invoke', async (p, signal) => {
    const fn = plugin.actions?.[p.action]
    if (!fn) throw new RpcError(CODE_METHOD_NOT_FOUND, 'unknown action: ' + p.action)
    return (await fn(p.params ?? {}, p.context ?? {}, host, signal)) ?? {}
  })
  conn.handle('triggers.set', async (p) => {
    host.triggers = p.triggers ?? []
    return { errors: (await plugin.triggers?.(host.triggers, host)) ?? {} }
  })
  conn.handle('options.list', async (p, signal) => ({
    options: (await plugin.options?.(p.action ?? '', p.field ?? '', host, signal)) ?? [],
  }))
  conn.handle('ingress.request', async (p) => {
    if (!plugin.ingress) return { status: 404 }
    const resp = await plugin.ingress({ ...p, body: Buffer.from(p.body ?? '', 'base64') }, host)
    const body = resp.body === undefined ? undefined : Buffer.from(resp.body as any).toString('base64')
    return { ...resp, body }
  })
  conn.handle('setup.run', async (p, signal) => {
    if (!plugin.setup) throw new Error('this plugin has no setup wizard')
    try {
      return { config: await plugin.setup(new Setup(host, conn, p.existing ?? {}, signal), signal) }
    } catch (e) {
      if (e instanceof CancelledError) throw new RpcError(CODE_CANCELLED, 'cancelled')
      throw e
    }
  })

  conn.onClose = () => {
    stop().finally(() => process.exit(0))
  }
}
