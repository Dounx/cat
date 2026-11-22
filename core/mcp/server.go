// Package mcp lets an AI use the hub: it serves the hub's connections,
// events and rules as tools over the Model Context Protocol (streamable
// HTTP, answered in plain JSON), and installs that server into the harnesses
// found on this machine.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

// ServerName is what the server is called in a harness's configuration.
const ServerName = "cat"

// The protocol versions answered as asked for; a client that asks for
// another is offered the first.
var versions = []string{"2025-06-18", "2025-11-25", "2025-03-26", "2024-11-05"}

const instructions = `Cat 是一个事件中心：聊天软件、订阅、定时等「接入」发出事件，「规则」按条件判断后触发动作（发消息、交给 AI、发 HTTP 请求……）。
- 要发消息或做别的事：先用 list_connections 看有哪些接入和它们的动作，再用 invoke_action 调用。发到哪个会话，从 list_events 里最近的事件取 conversation.id。
- 要了解发生过什么：list_events 看收到的事件，list_runs 看规则对它们做了什么。
- 要新建或修改规则：先用 list_rules 看现有规则的写法，可用 try_rule 拿历史事件或模拟事件先试运行草稿（建议先设 dry_run: true 演练，不保存规则也不真实执行动作），确认无误后再用 save_rule 保存；也可使用 replay_event 重放历史事件。`

type Server struct {
	Engine  *engine.Engine
	Version string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ServeHTTP answers one POSTed JSON-RPC message, or a batch of them. The
// server never speaks first, so there is no stream to GET.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "the MCP endpoint takes POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var out any
	if trimmed := strings.TrimSpace(string(body)); strings.HasPrefix(trimmed, "[") {
		var batch []request
		if err := json.Unmarshal(body, &batch); err != nil {
			out = response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}}
		} else {
			var list []response
			for _, req := range batch {
				if resp := s.handle(r.Context(), req); resp != nil {
					list = append(list, *resp)
				}
			}
			if len(list) > 0 {
				out = list
			}
		}
	} else {
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			out = response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}}
		} else if resp := s.handle(r.Context(), req); resp != nil {
			out = *resp
		}
	}
	if out == nil { // notifications only: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handle answers one message; nil for a notification.
func (s *Server) handle(ctx context.Context, req request) *response {
	if len(req.ID) == 0 {
		return nil
	}
	resp := &response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := versions[0]
		if slices.Contains(versions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "title": "Cat 事件中心", "version": s.Version},
			"instructions":    instructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		list := make([]map[string]any, len(tools))
		for i, t := range tools {
			list[i] = map[string]any{"name": t.name, "description": t.about, "inputSchema": t.schema}
		}
		resp.Result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			break
		}
		i := slices.IndexFunc(tools, func(t tool) bool { return t.name == p.Name })
		if i < 0 {
			resp.Error = &rpcError{-32602, "unknown tool: " + p.Name}
			break
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		out, err := tools[i].run(ctx, s, p.Arguments)
		if err != nil {
			// what the tool was given is wrong, or what it did failed: said
			// to the model, which can try again
			resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}
			break
		}
		text, _ := json.MarshalIndent(out, "", "  ")
		resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return resp
}

type tool struct {
	name, about string
	schema      map[string]any
	run         func(ctx context.Context, s *Server, args map[string]any) (any, error)
}

func object(required []string, props map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func prop(kind, about string) map[string]any {
	return map[string]any{"type": kind, "description": about}
}

func str(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

func limit(args map[string]any, def, max int) int {
	n, _ := args["limit"].(float64)
	if n < 1 {
		return def
	}
	return min(int(n), max)
}

const ruleFormat = `规则是一张图（graph）：nodes 里是节点，edges 里是连线 {from, to, branch?}。
节点都有 id 和 kind：
- kind "trigger"：sources（接入的 id 列表，空表示所有接入）、types（事件类型列表，可用 * 通配，空表示任何事件）、可选的 params（向接入订阅的内容，见该插件的 trigger_params）。
- kind "condition"：condition 是 expr 表达式（可用 payload、actor、conversation、type、source、steps.<id>、loop），成立走不带 branch 的线，不成立走 branch 为 "else" 的线。带 choices 时表达式算出其中一个选项，走 branch 等于该选项的线。也可以交给模型判断：填 instance（有 judge 动作的接入）和 params.prompt。
- kind "action"：instance（接入 id，或 "$source" 表示事件来的那个接入）、action（动作名）、params（参数，字符串里用 {{ payload.text }}、{{ steps.<id>.text }} 引用事件和前面节点的输出）。可选 retries、retry_wait_seconds、continue_on_error、timeout_seconds。
例：{"name":"转发","enabled":true,"graph":{"nodes":[{"id":"t1","kind":"trigger","sources":["telegram-ab12"],"types":["message.created"]},{"id":"s1","kind":"action","instance":"$source","action":"reply","params":{"text":"收到：{{ payload.text }}"}}],"edges":[{"from":"t1","to":"s1"}]}}`

var tools = []tool{
	{
		name:   "list_connections",
		about:  "列出 Cat 里的接入（聊天渠道、监控、AI、内置动作）：id、名字、状态、能做的动作及其参数、会发出的事件。调用 invoke_action 或写规则之前先看这个。",
		schema: object(nil, map[string]any{}),
		run: func(_ context.Context, s *Server, _ map[string]any) (any, error) {
			var out []map[string]any
			for _, inst := range s.Engine.Instances() {
				m, _ := s.Engine.Plugin(inst.Plugin)
				item := map[string]any{"id": inst.ID, "name": inst.Name, "plugin": m.Name, "kind": m.Category, "status": inst.Status}
				if inst.Error != "" {
					item["error"] = inst.Error
				}
				if m.Description != "" {
					item["about"] = m.Description
				}
				if len(m.Actions) > 0 {
					item["actions"] = m.Actions
				}
				if len(m.Events) > 0 {
					item["events"] = m.Events
				}
				if m.TriggerParams != nil {
					item["trigger_params"] = m.TriggerParams
				}
				out = append(out, item)
			}
			return out, nil
		},
	},
	{
		name:  "invoke_action",
		about: "让一个接入做一个动作，比如往某个会话发消息。动作会真的执行。instance 和 action 取自 list_connections，params 按那个动作的参数填。返回动作的输出。",
		schema: object([]string{"instance", "action"}, map[string]any{
			"instance": prop("string", "接入的 id"),
			"action":   prop("string", "动作名"),
			"params":   map[string]any{"type": "object", "description": "动作的参数"},
		}),
		run: func(ctx context.Context, s *Server, args map[string]any) (any, error) {
			params, _ := args["params"].(map[string]any)
			if params == nil {
				params = map[string]any{}
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			out, err := s.Engine.Invoke(ctx, str(args, "instance"), str(args, "action"), params, plugin.InvokeContext{})
			if err != nil {
				return nil, fmt.Errorf("动作失败：%w", err)
			}
			if out == nil {
				out = map[string]any{}
			}
			return out, nil
		},
	},
	{
		name:  "list_events",
		about: "列出 Cat 最近收到的事件，新的在前：时间、来自哪个接入、类型、会话、发送者和内容。可按接入或类型筛选，用 before 往前翻页。",
		schema: object(nil, map[string]any{
			"source": prop("string", "只看这个接入（id）的"),
			"type":   prop("string", "只看这个类型的，如 message.created"),
			"before": prop("string", "只看这个事件 id 之前的"),
			"limit":  prop("number", "最多几条，默认 20，最多 100"),
		}),
		run: func(_ context.Context, s *Server, args map[string]any) (any, error) {
			list, err := s.Engine.Store.ListEvents(store.EventFilter{
				Source: str(args, "source"), Type: str(args, "type"), Before: str(args, "before"), Limit: limit(args, 20, 100),
			})
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(list))
			for i := range list {
				out[i] = brief(&list[i])
			}
			return out, nil
		},
	},
	{
		name:   "get_event",
		about:  "取一个事件的全部数据，包括平台给的原始内容（raw）。",
		schema: object([]string{"id"}, map[string]any{"id": prop("string", "事件 id")}),
		run: func(_ context.Context, s *Server, args map[string]any) (any, error) {
			ev, err := s.Engine.Store.GetEvent(str(args, "id"))
			if err != nil {
				return nil, errors.New("没有这个事件")
			}
			return ev, nil
		},
	},
	{
		name:   "list_rules",
		about:  "列出 Cat 里的全部规则，连同它们的图（节点和连线）。写新规则前先看现有的是怎么写的。",
		schema: object(nil, map[string]any{}),
		run: func(_ context.Context, s *Server, _ map[string]any) (any, error) {
			list, err := s.Engine.Rules()
			if err != nil {
				return nil, err
			}
			out := make([]any, len(list))
			for i, r := range list {
				out[i] = bare(r)
			}
			return out, nil
		},
	},
	{
		name:  "save_rule",
		about: "新建或修改一条规则。rule.id 为空是新建，否则整条替换那条规则（先用 list_rules 取到它，改完整条传回）。保存后立即生效。\n" + ruleFormat,
		schema: object([]string{"rule"}, map[string]any{
			"rule": map[string]any{"type": "object", "description": "整条规则：name、enabled、graph{nodes, edges}，修改时带上 id"},
		}),
		run: func(_ context.Context, s *Server, args map[string]any) (any, error) {
			raw, _ := json.Marshal(args["rule"])
			var r rules.Rule
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, fmt.Errorf("规则的格式不对：%w", err)
			}
			if r.Graph == nil {
				return nil, errors.New("规则要有 graph（nodes 和 edges）")
			}
			if r.ID != "" {
				list, _ := s.Engine.Store.ListRules()
				i := slices.IndexFunc(list, func(x rules.Rule) bool { return x.ID == r.ID })
				if i < 0 {
					return nil, errors.New("没有 id 为 " + r.ID + " 的规则；新建规则时不要填 id")
				}
				keepLayout(&r, &list[i])
			}
			saved, err := s.Engine.SaveRule(r)
			if err != nil {
				return nil, err
			}
			s.Engine.Publish("rules", map[string]string{"rule": saved.ID})
			return bare(engine.RuleView{Rule: saved}), nil
		},
	},
	{
		name:  "set_rule_enabled",
		about: "启用或停用一条规则。",
		schema: object([]string{"id", "enabled"}, map[string]any{
			"id": prop("string", "规则 id"), "enabled": prop("boolean", "true 启用，false 停用"),
		}),
		run: func(_ context.Context, s *Server, args map[string]any) (any, error) {
			list, _ := s.Engine.Store.ListRules()
			i := slices.IndexFunc(list, func(x rules.Rule) bool { return x.ID == str(args, "id") })
			if i < 0 {
				return nil, errors.New("没有这条规则")
			}
			list[i].Enabled, _ = args["enabled"].(bool)
			saved, err := s.Engine.SaveRule(list[i])
			if err != nil {
				return nil, err
			}
			s.Engine.Publish("rules", map[string]string{"rule": saved.ID})
			return map[string]any{"id": saved.ID, "name": saved.Name, "enabled": saved.Enabled}, nil
		},
	},
	{
		name:   "delete_rule",
		about:  "删除一条规则。删了不能恢复。",
		schema: object([]string{"id"}, map[string]any{"id": prop("string", "规则 id")}),
		run: func(_ context.Context, s *Server, args map[string]any) (any, error) {
			list, _ := s.Engine.Store.ListRules()
			i := slices.IndexFunc(list, func(x rules.Rule) bool { return x.ID == str(args, "id") })
			if i < 0 {
				return nil, errors.New("没有这条规则")
			}
			if err := s.Engine.DeleteRule(list[i].ID); err != nil {
				return nil, err
			}
			s.Engine.Publish("rules", map[string]string{"rule": list[i].ID})
			return map[string]any{"deleted": list[i].Name}, nil
		},
	},
	{
		name:  "list_runs",
		about: "列出规则的运行记录，新的在前：哪条规则、为哪个事件、成功还是失败、每一步的参数、输出和错误。可按规则或事件筛选。",
		schema: object(nil, map[string]any{
			"rule_id":  prop("string", "只看这条规则的"),
			"event_id": prop("string", "只看这个事件触发的"),
			"limit":    prop("number", "最多几条，默认 10，最多 50"),
		}),
		run: func(_ context.Context, s *Server, args map[string]any) (any, error) {
			list, err := s.Engine.Store.ListRuns(str(args, "rule_id"), str(args, "event_id"), limit(args, 10, 50))
			if err != nil {
				return nil, err
			}
			for i := range list { // the rule as it stood, and the path, are for drawing
				list[i].Rule, list[i].Trace = nil, nil
			}
			return list, nil
		},
	},
	{
		name:  "try_rule",
		about: "拿一个历史事件或模拟事件试运行一条规则（传入未保存的 rule 对象，或已保存的 rule_id），等待结果返回。不会保存规则；设置 dry_run: true 可进入演练模式（只检查触发、条件与参数模板，不真实执行动作，也不写入运行记录）。",
		schema: object(nil, map[string]any{
			"event_id":     prop("string", "用来试运行的历史事件 id（与 event 二选一）"),
			"event":        map[string]any{"type": "object", "description": "自定义模拟事件对象（与 event_id 二选一），可包含 source、type、payload、actor、conversation 等字段，不会写入事件库"},
			"dry_run":      prop("boolean", "true 表示演练模式：不真实调用动作/模型，仅返回走过的路径和渲染后的参数"),
			"mock_outputs": map[string]any{"type": "object", "description": "演练模式下按节点 id 提供的模拟输出，例如 {\"s1\":{\"text\":\"ok\"}}（可选）"},
			"rule_id":      prop("string", "已保存规则的 id（与 rule 二选一）"),
			"rule":         map[string]any{"type": "object", "description": "未保存或改过的规则对象：name、graph{nodes, edges}（与 rule_id 二选一）"},
		}),
		run: func(_ context.Context, s *Server, args map[string]any) (any, error) {
			eventID := str(args, "event_id")
			var mockEv *event.Event
			if args["event"] != nil {
				raw, _ := json.Marshal(args["event"])
				var ev event.Event
				if err := json.Unmarshal(raw, &ev); err != nil {
					return nil, fmt.Errorf("模拟事件格式不对：%w", err)
				}
				mockEv = &ev
			}
			if eventID == "" && mockEv == nil {
				return nil, errors.New("请提供 event_id 或 event")
			}
			var r rules.Rule
			if args["rule"] != nil {
				raw, _ := json.Marshal(args["rule"])
				if err := json.Unmarshal(raw, &r); err != nil {
					return nil, fmt.Errorf("规则的格式不对：%w", err)
				}
				if r.Graph == nil && len(r.Steps) == 0 {
					return nil, errors.New("规则要有 graph（nodes 和 edges）")
				}
			} else if rid := str(args, "rule_id"); rid != "" {
				list, _ := s.Engine.Store.ListRules()
				i := slices.IndexFunc(list, func(x rules.Rule) bool { return x.ID == rid })
				if i < 0 {
					return nil, errors.New("没有 id 为 " + rid + " 的规则")
				}
				r = list[i]
			} else {
				return nil, errors.New("请提供 rule 或 rule_id")
			}
			dryRun, _ := args["dry_run"].(bool)
			var mockOutputs map[string]map[string]any
			if args["mock_outputs"] != nil {
				raw, _ := json.Marshal(args["mock_outputs"])
				if err := json.Unmarshal(raw, &mockOutputs); err != nil {
					return nil, fmt.Errorf("mock_outputs 格式不对：%w", err)
				}
			}
			run, err := s.Engine.TryRuleWithOptions(r, engine.TryOptions{
				EventID:     eventID,
				Event:       mockEv,
				DryRun:      dryRun,
				MockOutputs: mockOutputs,
			})
			if err != nil {
				return nil, err
			}
			if run == nil {
				return map[string]any{"matched": false}, nil
			}
			copied := *run
			copied.Rule, copied.Trace = nil, nil
			return map[string]any{"matched": true, "run": copied}, nil
		},
	},
	{
		name:  "replay_event",
		about: "把一个历史事件重新交给规则跑一遍。不填 rule_id 时交给所有匹配且已启用的规则，填了则只跑那一条（即使停用也能跑）。",
		schema: object([]string{"event_id"}, map[string]any{
			"event_id": prop("string", "要重放的历史事件 id"),
			"rule_id":  prop("string", "只对这条规则重放（可选）"),
		}),
		run: func(_ context.Context, s *Server, args map[string]any) (any, error) {
			eventID := str(args, "event_id")
			if eventID == "" {
				return nil, errors.New("缺少 event_id")
			}
			n, err := s.Engine.Replay(eventID, str(args, "rule_id"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"matched": n}, nil
		},
	},
}

// keepLayout gives a rule that comes back changed what only the editor reads
// and list_rules left out: where the nodes it still has stand on the canvas.
// A node that is new has no place yet, and the editor then lays the graph out
// afresh.
func keepLayout(r, old *rules.Rule) {
	if r.Meta == nil {
		r.Meta = old.Meta
	}
	if old.Graph == nil {
		return
	}
	for i := range r.Graph.Nodes {
		n := &r.Graph.Nodes[i]
		j := slices.IndexFunc(old.Graph.Nodes, func(o rules.Node) bool { return o.ID == n.ID && o.Kind == n.Kind })
		if j >= 0 && n.Meta == nil {
			n.Meta = old.Graph.Nodes[j].Meta
		}
	}
}

// brief is an event without what the platform sent as it was: enough to see
// what came in, and little enough to list.
func brief(ev *event.Event) map[string]any {
	m := ev.Map()
	delete(m, "raw")
	delete(m, "triggers")
	return m
}

// bare is a rule without what only the editor reads: where its nodes stand
// on the canvas, and the rows of its condition builders.
func bare(r engine.RuleView) map[string]any {
	raw, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	delete(m, "meta")
	if g, ok := m["graph"].(map[string]any); ok {
		nodes, _ := g["nodes"].([]any)
		for _, n := range nodes {
			if node, ok := n.(map[string]any); ok {
				delete(node, "meta")
			}
		}
		delete(m, "steps")
		delete(m, "condition")
	}
	return m
}
