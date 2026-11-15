// A rule as it is handed from one hub to another: the graph, and what each
// connection it names is, so that the hub taking it in can put its own in
// their place. A connection is an instance, and its id means nothing
// elsewhere; the plugin behind it does.
import type { GraphNode, Instance, Plugin, Rule } from './types'

export const SHARE_FORMAT = 'cat-rule'
const SOURCE = '$source'

export interface Shared {
  format: typeof SHARE_FORMAT
  version: 1
  name: string
  graph: NonNullable<Rule['graph']>
  // the connections the rule names, by the id it knows them under
  uses: Record<string, { plugin: string; name: string }>
}

// The ids of the instances a node names: what a trigger listens to, and what
// an action or a condition is put to.
const namedBy = (n: GraphNode) => [...(n.sources || []), n.instance].filter((id) => id && id !== SOURCE)

// A rule ready to be handed on. One saved before graphs existed is not: open
// it in the editor and save it first.
export function exportRule(rule: Rule, instances: Instance[]): Shared {
  if (!rule.graph) throw new Error('这条规则还是旧的写法，先在编辑器里打开并保存一次')
  const uses: Shared['uses'] = {}
  for (const n of rule.graph.nodes)
    for (const id of namedBy(n)) {
      const inst = instances.find((i) => i.id === id)
      if (inst) uses[id] = { plugin: inst.plugin, name: inst.name }
    }
  return { format: SHARE_FORMAT, version: 1, name: rule.name, graph: rule.graph, uses }
}

export function parseShared(text: string): Shared {
  let data: any
  try {
    data = JSON.parse(text)
  } catch {
    throw new Error('这不是一段 JSON，要粘贴导出的规则的全部内容')
  }
  if (data?.format !== SHARE_FORMAT || !Array.isArray(data.graph?.nodes) || !Array.isArray(data.graph?.edges))
    throw new Error('这不是 Cat 导出的规则')
  if (data.version !== 1) throw new Error('这条规则是更新版本的 Cat 导出的，这一版读不了')
  return { ...data, name: String(data.name || ''), uses: data.uses || {} }
}

// One connection a shared rule names, and what there is here to stand in
// for it.
export interface Need {
  id: string // as the shared rule knows it
  plugin: string
  name: string
  // the instances here of the same plugin; empty when one has to be connected
  fits: Instance[]
  // the plugin itself, when it is installed here
  installed?: Plugin
}

// What a shared rule needs connected here. One that comes ready with its
// plugin (HTTP 请求, RSS…) is there under the same id everywhere and is not
// asked about.
export function needsOf(shared: Shared, instances: Instance[], plugins: Plugin[]): Need[] {
  const given = (id: string, plugin: string) => {
    const p = plugins.find((p) => p.id === plugin)
    return id === plugin && !!p && (!!p.singleton || !!p.default_instance) && instances.some((i) => i.id === id)
  }
  return Object.entries(shared.uses)
    .filter(([id, u]) => !given(id, u.plugin))
    .map(([id, u]) => ({
      id,
      plugin: u.plugin,
      name: u.name,
      fits: instances.filter((i) => i.plugin === u.plugin),
      installed: plugins.find((p) => p.id === u.plugin),
    }))
}

// The stand-in to start from for each need: the instance of the same id when
// this is the hub the rule came from, else the only one that fits.
export function suggest(needs: Need[]): Record<string, string> {
  const chosen: Record<string, string> = {}
  for (const n of needs)
    chosen[n.id] = (n.fits.find((i) => i.id === n.id) || (n.fits.length === 1 ? n.fits[0] : undefined))?.id || ''
  return chosen
}

// The rule a shared one becomes here: new, with the chosen instances in
// place of the ones it named. A connection nothing was chosen for stays as
// it was named; the editor shows it as missing.
export function importRule(shared: Shared, chosen: Record<string, string>): Rule {
  const swap = (id: string) => chosen[id] || id
  const nodes = shared.graph.nodes.map((n) => ({
    ...n,
    instance: n.instance ? swap(n.instance) : n.instance,
    ...(n.sources ? { sources: n.sources.map(swap) } : {}),
  }))
  return {
    id: '',
    name: shared.name,
    enabled: true,
    sources: [],
    types: [],
    condition: '',
    steps: [],
    graph: { nodes, edges: shared.graph.edges },
  }
}

// A file name for a rule: its name, without what a file system minds.
export const fileName = (name: string) =>
  (name.replace(/[\\/:*?"<>|\s]+/g, '-').replace(/^-+|-+$/g, '') || 'rule') + '.cat-rule.json'
