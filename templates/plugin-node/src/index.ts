// A minimal Cat plugin in TypeScript.
//
// During development plugin.json runs it with `bun run src/index.ts`. To ship
// it, `bun run build` produces a single executable; then change "command" in
// plugin.json to ["./cat-plugin-hello-node"].
//
// Never write to stdout (console.log): it carries the protocol. Use host.log().
import { run } from '@cat-hub/plugin-sdk'

run({
  // Runs once per instance. Resolve when ready; `signal` aborts on stop.
  start(host, signal) {
    const seconds = Number(host.config.interval) > 0 ? Number(host.config.interval) : 60
    let count = 0
    const timer = setInterval(() => {
      host.emit({ type: 'hello.tick', payload: { count: ++count } }).catch((e) => host.log('error', String(e)))
    }, seconds * 1000)
    signal.addEventListener('abort', () => clearInterval(timer))
  },
  actions: {
    // The returned object is the step's output: later steps read it as
    // {{ steps.<id>.text }}.
    greet(params, _ctx, host) {
      return { text: `${host.config.greeting || '你好'}，${params.name}！` }
    },
  },
})
