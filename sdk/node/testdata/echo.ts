// The Node counterpart of the Go echo test plugin: the host's conformance
// tests run both through the same scenarios.
import { ActionError, CancelledError, run } from '../src/index.ts'

run({
  async start(host) {
    if (host.config.fail === 'yes') throw new Error('configured to fail')
    const starts = Number((await host.stateGet('starts')) ?? 0) + 1
    await host.stateSet('starts', String(starts))
    await host.emit({ type: 'echo.started', payload: { starts, greeting: host.config.greeting ?? '' } })
  },
  actions: {
    async echo(params, ctx, host) {
      const event_id = await host.emit({
        type: 'echo.said', payload: { text: params.text },
        actor: { id: 'echo' }, conversation: { id: 'c1', kind: 'private' },
      })
      return { text: params.text, event_id, trigger: ctx.event?.type ?? '' }
    },
    fail() { throw new ActionError('boom', { partial: true }) },
    crash() { process.exit(7) },
    wait(_p, _c, _h, signal) {
      return new Promise((_, reject) => signal.addEventListener('abort', () => reject(new Error('aborted'))))
    },
  },
  ingress(req) {
    return { status: 200, body: 'got:' + Buffer.from(req.body).toString() }
  },
  async setup(s) {
    await s.progress('preparing', 0.5)
    const out = await s.ask({ kind: 'form', title: 'Greeting', schema: { type: 'object', properties: { greeting: { type: 'string' } } } })
    if (!(await s.confirm('Save?'))) throw new CancelledError()
    return { greeting: out.greeting }
  },
})
