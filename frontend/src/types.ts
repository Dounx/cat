export interface Schema {
  type?: string
  title?: string
  description?: string
  format?: string
  enum?: string[]
  default?: unknown
  required?: string[]
  'x-order'?: string[]
  properties?: Record<string, Schema>
}
export interface ActionSpec {
  name: string
  title: string
  description?: string
  params?: Schema
}
export interface Plugin {
  id: string
  name: string
  version: string
  description?: string
  icon?: string
  category: 'channel' | 'monitor' | 'processor'
  capabilities: string[]
  config_schema?: Schema
  actions?: ActionSpec[]
  events?: { type: string; title: string }[]
  singleton?: boolean
  ingress?: boolean
  setup?: boolean
  builtin: boolean
}
export interface Instance {
  id: string
  plugin: string
  name: string
  enabled: boolean
  config: Record<string, unknown>
  status: string
  error?: string
}
export interface HubEvent {
  id: string
  source: string
  plugin: string
  type: string
  time: string
  conversation?: { id: string; kind?: string; name?: string }
  actor?: { id: string; name?: string; is_bot?: boolean }
  payload: Record<string, unknown>
  raw?: unknown
}
export interface Step {
  id: string
  instance: string
  action: string
  params?: Record<string, unknown>
  continue_on_error?: boolean
  timeout_seconds?: number
}
// A rule drawn as a graph: trigger, condition and action nodes wired together.
// A trigger node names the events that start there, and a throttle node
// lets an event past once per window. An edge leaving a
// condition with branch "else" is taken when the condition does not hold; one
// whose `from` is "trigger" starts at the rule's own sources and types, in a
// graph saved without trigger nodes.
export interface GraphNode extends Step {
  kind: 'trigger' | 'condition' | 'throttle' | 'action'
  condition?: string
  sources?: string[]
  types?: string[]
  throttle_seconds?: number
  throttle_key?: string
  meta?: Record<string, any>
}
export interface GraphEdge {
  from: string
  to: string
  branch?: string
}
export interface Rule {
  id: string
  name: string
  enabled: boolean
  sources?: string[]
  types?: string[]
  condition?: string
  steps: Step[]
  graph?: { nodes: GraphNode[]; edges: GraphEdge[] }
  throttle_seconds?: number
  throttle_key?: string
  meta?: Record<string, any>
  error?: string
}
export interface StepResult {
  id: string
  instance: string
  action: string
  params?: Record<string, unknown>
  output?: Record<string, unknown>
  error?: string
  duration_ms: number
}
export interface Run {
  id: string
  rule_id: string
  rule_name: string
  event_id: string
  status: string
  started_at: string
  finished_at?: string
  steps: StepResult[]
  replay?: boolean
}
