// cat-plugin-discord: connects Cat to Discord through a bot's Gateway
// connection (events) and the REST API (actions). No dependencies beyond the
// SDK: the Gateway protocol is small enough to speak directly.
import { CancelledError, run, type ActionContext, type CatEvent, type Host, type Setup } from '@cat-hub/plugin-sdk'

const DEFAULT_API = 'https://discord.com/api/v10'
const MAX_TEXT = 2000
// GUILDS | GUILD_MESSAGES | DIRECT_MESSAGES | MESSAGE_CONTENT
const INTENTS = (1 << 0) | (1 << 9) | (1 << 12) | (1 << 15)
// VIEW_CHANNEL | SEND_MESSAGES | READ_MESSAGE_HISTORY
const INVITE_PERMISSIONS = 1024 + 2048 + 65536

const FATAL_CLOSE: Record<number, string> = {
  4004: 'Bot Token 无效',
  4013: 'Gateway intents 无效',
  4014: '未开启 Message Content Intent：请到 Discord 开发者后台 → Bot → Privileged Gateway Intents 打开它',
}

class Api {
  readonly base: string
  constructor(
    readonly token: string,
    base?: string,
  ) {
    this.base = (base || DEFAULT_API).replace(/\/+$/, '')
  }
  async call<T = any>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    for (let attempt = 0; ; attempt++) {
      const res = await fetch(this.base + path, {
        method,
        signal,
        headers: {
          Authorization: 'Bot ' + this.token,
          'Content-Type': 'application/json',
          'User-Agent': 'DiscordBot (cat-event-hub, 0.1)',
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      })
      if (res.status === 429 && attempt < 3) {
        // rate limited: wait as told, then retry
        const info: any = await res.json().catch(() => ({}))
        await new Promise((r) => setTimeout(r, Math.min(Number(info.retry_after ?? 1), 10) * 1000))
        continue
      }
      if (res.status === 401) throw new Error('Bot Token 无效')
      if (!res.ok) {
        const info: any = await res.json().catch(() => ({}))
        throw new Error(`Discord ${res.status}: ${info.message ?? res.statusText}`)
      }
      return res.status === 204 ? (undefined as T) : ((await res.json()) as T)
    }
  }
}

/** Cuts text into pieces of at most `max` characters, preferring line breaks. */
export function split(text: string, max: number): string[] {
  const chars = [...text]
  const out: string[] = []
  while (chars.length > max) {
    let cut = max
    for (let i = max; i > max / 2; i--) {
      if (chars[i - 1] === '\n') {
        cut = i
        break
      }
    }
    out.push(chars.splice(0, cut).join(''))
  }
  out.push(chars.join(''))
  return out
}

class Bot {
  private api: Api
  private me: { id: string; username: string } = { id: '', username: '' }
  private ws?: WebSocket
  private heartbeat?: ReturnType<typeof setInterval>
  private seq: number | null = null
  private sessionId = ''
  private resumeUrl = ''
  private gatewayUrl = ''
  private backoff = 1000
  private healthy = false

  constructor(
    private host: Host,
    private signal: AbortSignal,
  ) {
    this.api = new Api(String(host.config.token ?? '').trim(), host.config.api_base)
  }

  async start(): Promise<void> {
    if (!this.api.token) throw new Error('缺少 Bot Token')
    this.me = await this.api.call('GET', '/users/@me', undefined, this.signal)
    this.gatewayUrl = (await this.api.call<{ url: string }>('GET', '/gateway/bot', undefined, this.signal)).url
    this.signal.addEventListener('abort', () => this.close())
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('连接 Discord Gateway 超时')), 30_000)
      this.connect(
        () => {
          clearTimeout(timer)
          resolve()
        },
        (e) => {
          clearTimeout(timer)
          reject(e)
        },
      )
    })
    this.host.log('info', `已连接 ${this.me.username}`)
  }

  private close() {
    clearInterval(this.heartbeat)
    try {
      this.ws?.close(1000)
    } catch {
      /* already closed */
    }
  }

  // connect opens the Gateway socket. onReady / onFatal are only supplied for
  // the first connection, to settle start().
  private connect(onReady?: () => void, onFatal?: (e: Error) => void) {
    const resuming = !!(this.sessionId && this.resumeUrl)
    const url = (resuming ? this.resumeUrl : this.gatewayUrl).replace(/\/+$/, '') + '/?v=10&encoding=json'
    const ws = new WebSocket(url)
    this.ws = ws
    let acked = true

    ws.onmessage = (m) => {
      const msg = JSON.parse(String(m.data))
      if (msg.s != null) this.seq = msg.s
      switch (msg.op) {
        case 10: {
          // Hello
          clearInterval(this.heartbeat)
          this.heartbeat = setInterval(() => {
            if (!acked) return ws.close(4000) // zombied connection: reconnect
            acked = false
            ws.send(JSON.stringify({ op: 1, d: this.seq }))
          }, msg.d.heartbeat_interval)
          ws.send(
            JSON.stringify(
              resuming
                ? { op: 6, d: { token: this.api.token, session_id: this.sessionId, seq: this.seq } }
                : {
                    op: 2,
                    d: {
                      token: this.api.token,
                      intents: INTENTS,
                      properties: { os: process.platform, browser: 'cat', device: 'cat' },
                    },
                  },
            ),
          )
          break
        }
        case 11:
          acked = true
          break
        case 1:
          ws.send(JSON.stringify({ op: 1, d: this.seq }))
          break
        case 7:
          ws.close(4000)
          break // server asks us to reconnect
        case 9: // invalid session
          if (!msg.d) {
            this.sessionId = ''
            this.seq = null
          }
          ws.close(4000)
          break
        case 0:
          this.dispatch(msg.t, msg.d, onReady)
          break
      }
    }
    ws.onclose = (ev) => {
      clearInterval(this.heartbeat)
      if (this.signal.aborted) return
      const fatal = FATAL_CLOSE[ev.code]
      if (fatal) {
        this.host.setStatus('error', fatal)
        onFatal?.(new Error(fatal))
        return
      }
      if (this.healthy) {
        this.healthy = false
        this.host.setStatus('error', '与 Discord 的连接中断，正在重连')
      }
      setTimeout(() => this.connect(onReady, onFatal), this.backoff)
      this.backoff = Math.min(this.backoff * 2, 60_000)
    }
    ws.onerror = () => {
      /* onclose follows */
    }
  }

  private dispatch(type: string, d: any, onReady?: () => void) {
    if (type === 'READY' || type === 'RESUMED') {
      if (type === 'READY') {
        this.sessionId = d.session_id
        this.resumeUrl = d.resume_gateway_url ?? ''
      }
      this.backoff = 1000
      if (!this.healthy) {
        this.healthy = true
        this.host.setStatus('running')
      }
      onReady?.()
      return
    }
    if (type !== 'MESSAGE_CREATE' && type !== 'MESSAGE_UPDATE') return
    if (!d.author || d.author.id === this.me.id) return // ignore our own messages
    const text: string = d.content ?? ''
    const event: CatEvent = {
      type: type === 'MESSAGE_CREATE' ? 'message.created' : 'message.edited',
      time: d.timestamp,
      conversation: { id: d.channel_id, kind: d.guild_id ? 'group' : 'private' },
      actor: { id: d.author.id, name: d.author.global_name || d.author.username, is_bot: !!d.author.bot },
      payload: {
        text,
        message_id: d.id,
        mentioned: !d.guild_id || (d.mentions ?? []).some((u: any) => u.id === this.me.id),
        username: d.author.username,
        ...(d.guild_id ? { guild_id: d.guild_id } : {}),
        ...(d.referenced_message ? { reply_to_message_id: d.referenced_message.id } : {}),
        ...(d.attachments?.length
          ? { attachments: d.attachments.map((a: any) => ({ kind: 'file', url: a.url, name: a.filename })) }
          : {}),
      },
      raw: d,
    }
    this.host.emit(event).catch((e) => this.host.log('error', 'emit: ' + e.message))
  }

  async send(channelId: string, text: unknown, replyTo: string | undefined, signal: AbortSignal) {
    const body = typeof text === 'string' ? text : JSON.stringify(text ?? '')
    if (!body.trim()) throw new Error('消息内容为空')
    const ids: string[] = []
    for (const [i, part] of split(body, MAX_TEXT).entries()) {
      const sent = await this.api.call<{ id: string }>(
        'POST',
        `/channels/${channelId}/messages`,
        {
          content: part,
          ...(replyTo && i === 0 ? { message_reference: { message_id: replyTo, fail_if_not_exists: false } } : {}),
        },
        signal,
      )
      ids.push(sent.id)
    }
    return { message_id: ids[ids.length - 1], message_ids: ids, channel_id: channelId }
  }

  typing(channelId: string, signal: AbortSignal) {
    return this.api.call('POST', `/channels/${channelId}/typing`, undefined, signal)
  }
}

function conversationOf(ctx: ActionContext, action: string): string {
  const id = ctx.event?.conversation?.id
  if (!id) throw new Error(`「${action}」需要由一条聊天消息触发；要主动发消息请用「发送到指定频道」`)
  return id
}

async function setup(s: Setup): Promise<Record<string, unknown>> {
  const config: Record<string, unknown> = { ...s.existing }
  let problem = ''
  for (;;) {
    const steps =
      '1. 打开 Discord 开发者后台，新建一个 Application。\n2. 进入 Bot 页面，点 Reset Token 并复制。\n3. 在同一页的 Privileged Gateway Intents 里打开 Message Content Intent。\n4. 把 Token 粘贴到下面。'
    const out = await s.ask({
      kind: 'form',
      title: '连接 Discord Bot',
      url: 'https://discord.com/developers/applications',
      description: problem ? `⚠ ${problem}\n\n${steps}` : steps,
      schema: {
        type: 'object',
        required: ['token'],
        properties: { token: { type: 'string', title: 'Bot Token', format: 'password' } },
      },
    })
    config.token = out.token
    await s.progress('正在验证 Token…')
    try {
      const api = new Api(String(out.token ?? '').trim(), config.api_base as string | undefined)
      const me = await api.call<{ id: string; username: string }>('GET', '/users/@me')
      const app = await api.call<{ id: string }>('GET', '/oauth2/applications/@me').catch(() => ({ id: me.id }))
      const invite = `https://discord.com/oauth2/authorize?client_id=${app.id}&scope=bot&permissions=${INVITE_PERMISSIONS}`
      await s.ask({
        kind: 'confirm',
        title: `已验证 ${me.username}，把它邀请进服务器`,
        url: invite,
        description: '打开邀请链接，选择要加入的服务器并授权。完成后点确认。\n\n只用私信的话可以直接确认。',
      })
      return config
    } catch (e: any) {
      if (e instanceof CancelledError) throw e
      problem = e.message
    }
  }
}

let bot: Bot | undefined

run({
  async start(host, signal) {
    bot = new Bot(host, signal)
    await bot.start()
  },
  actions: {
    reply: (p, ctx, _h, signal) =>
      bot!.send(
        conversationOf(ctx, '回复'),
        p.text,
        p.quote === false ? undefined : (ctx.event?.payload?.message_id as string),
        signal,
      ),
    send: (p, _ctx, _h, signal) => {
      if (!p.channel_id) throw new Error('缺少频道 ID')
      return bot!.send(String(p.channel_id), p.text, undefined, signal)
    },
    typing: async (_p, ctx, _h, signal) => {
      await bot!.typing(conversationOf(ctx, '正在输入'), signal)
      return {}
    },
  },
  setup,
})
