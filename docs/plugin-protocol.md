# Cat 插件协议（v1）

插件是一个可执行程序。Cat 为每个配置好的实例启动一个插件进程，通过标准输入输出与它通信。任何语言都可以写插件；官方提供 Go 和 Node / TypeScript 两个 SDK，它们只是对本协议的封装。

- [事件信封](#事件信封)
- [plugin.json](#pluginjson)
- [传输](#传输)
- [生命周期](#生命周期)
- [方法一览](#方法一览)
- [聊天平台约定](#聊天平台约定)
- [Harness 约定](#harness-约定)
- [接入向导](#接入向导)
- [托管依赖](#托管依赖)

## 事件信封

所有来源的事件都被规范成同一个结构，规则和模板看到的就是它：

```jsonc
{
  "id": "…",                // Cat 分配
  "source": "telegram-1a2b", // 实例 ID，Cat 填写
  "plugin": "telegram",      // Cat 填写
  "type": "message.created",
  "time": "2026-11-10T09:00:00Z",
  "conversation": { "id": "-1001", "kind": "group", "name": "Ops" }, // kind: private | group | channel
  "actor": { "id": "42", "name": "Ann", "is_bot": false },
  "payload": { "text": "…", "message_id": "7" },
  "raw": { }                 // 平台原始数据，原样保留
}
```

插件只需要填 `type`，以及有意义时的 `conversation`、`actor`、`payload`、`raw`、`time`。`id`、`source`、`plugin` 由 Cat 决定，插件填了也会被覆盖。

## plugin.json

放在插件目录根部。Cat 在这些位置查找插件（每个含 `plugin.json` 的子目录是一个插件）：`--plugins` 指定的目录、`<数据目录>/plugins`、可执行文件旁的 `plugins`。

```jsonc
{
  "id": "telegram",            // 唯一标识，不含 / \ . 和空格
  "name": "Telegram",
  "version": "0.1.0",
  "protocol": 1,
  "description": "…",
  "command": ["./cat-plugin-telegram"], // 以 ./ 开头相对插件目录解析，否则走 PATH
  "category": "channel",       // "channel"（默认）、"monitor" 或 "processor"，见下
  "capabilities": ["source", "action"],
  "ingress": false,            // true：接收 /ingress/<实例 ID> 的 HTTP 请求
  "setup": true,               // true：提供接入向导
  "config_schema": { },        // JSON Schema，界面据此渲染配置表单
  "events": [ { "type": "message.created", "title": "收到消息" } ],
  "actions": [
    { "name": "reply", "title": "回复", "description": "…", "params": { /* JSON Schema */ } }
  ],
  "dependencies": [ ]          // 见「托管依赖」
}
```

`category` 说明插件在用户眼里扮演的角色，只影响界面上的分组，不限制它在规则里怎么用：

- `channel`（渠道，默认）：和外部世界的双向连接，消息从这里来、回复和通知发到这里去。例如 Telegram、QQ、Webhook。
- `monitor`（监控）：盯着某样东西，有变化时发出事件，自己没有地方可以“发消息”。例如 RSS 订阅、网页变动。要把变化通知给人，由规则把事件交给某个渠道的动作。
- `processor`（处理器）：收到事件后负责处理并给出结果。例如 Claude Code、大模型对话，也可以是关键词自动回复、转人工这类不涉及 AI 的实现。

填其它值插件会加载失败。

表单渲染支持的 Schema 子集：`type` 为 `string` / `number` / `integer` / `boolean` / `object`，以及 `title`、`description`、`default`、`enum`、`required`；`format: "password"` 和 `format: "textarea"`；`x-order` 数组指定字段顺序（JSON 对象本身无序）。

## 传输

- 标准输入输出上的 **JSON-RPC 2.0**，每条消息占一行（以 `\n` 结尾，消息内不得有换行）。
- 双向：Cat 和插件都可以发请求和通知。
- **标准输出只属于协议**，日志请用 `log` 通知或写标准错误（标准错误的每一行会被记为该实例的日志）。
- 取消：发起方放弃一个请求时发送通知 `$/cancel`，参数 `{ "id": <请求 id> }`，接收方应尽快中止对应处理。
- 错误对象可带 `data`。动作失败但有部分输出时，把输出放进 `error.data`，它会被记入运行记录。
- 错误码 `-32800` 表示用户取消（用于接入向导）。

## 生命周期

```
Cat                                   插件
 │ ── initialize ───────────────────▶ │  保存配置
 │ ◀─────────────── { protocol: 1 } ─ │
 │ ── start ────────────────────────▶ │  连接平台；就绪后才返回
 │ ◀──────────────────────── null ─── │  （返回错误 = 启动失败，Cat 不重试）
 │ ◀── event.emit / log / status ──── │  运行期间
 │ ── action.invoke ────────────────▶ │
 │ ── stop ─────────────────────────▶ │  随后 Cat 关闭标准输入；3 秒后强制结束进程
```

- `start` 返回错误（例如 Token 无效）时，实例显示为出错，等用户改配置。
- 进程在运行中意外退出时，Cat 以退避间隔（1 秒起，最长 1 分钟）自动重启它。
- 插件进程运行在独立的进程组里；停止实例时，它启动的子进程会被一并结束。

## 方法一览

### Cat → 插件

| 方法 | 参数 | 返回 |
|---|---|---|
| `initialize` | `{ protocol, mode: "run" \| "setup", instance: { id }, config, data_dir }` | `{ protocol }` |
| `start` | — | — |
| `stop` | — | — |
| `action.invoke` | `{ action, params, context: { event?, rule_id?, run_id? } }` | 一个对象，成为该步骤的输出 |
| `ingress.request` | `{ method, path, query, headers, body }`（`body` 为 base64） | `{ status, headers?, body? }`（`body` 为 base64） |
| `setup.run` | `{ existing }`（已有配置，新建时为空对象） | `{ config }` |

`data_dir` 是该实例私有的持久目录。`context.event` 是触发规则的那个事件。

### 插件 → Cat

| 方法 | 类型 | 参数 | 返回 |
|---|---|---|---|
| `event.emit` | 请求 | 事件 | `{ id }` |
| `log` | 通知 | `{ level, message }`，level 为 debug / info / warn / error | |
| `status` | 通知 | `{ status: "running" \| "error", message }`，`start` 返回后连接状态变化时用 | |
| `state.get` | 请求 | `{ key }` | `{ value, found }` |
| `state.set` | 请求 | `{ key, value }`（字符串；重启后仍在，实例删除时清除） | |
| `setup.show` | 请求 | 一个向导步骤 | 立即返回 |
| `setup.ask` | 请求 | 一个 `form` 或 `confirm` 步骤 | 用户的回答；用户关闭向导时返回错误 `-32800` |
| `host.dependency` | 请求 | `{ name }` | `{ path }` |

## 聊天平台约定

为了让一条规则能在不同平台间通用，聊天类插件应遵守：

- 收到消息发出 `message.created`（编辑为 `message.edited`），`payload` 至少包含 `text` 和 `message_id`（字符串），并尽量提供 `mentioned`（布尔：私聊，或消息提到了机器人）。
- 提供动作 `reply`，参数至少有 `text`：在 `context.event.conversation` 里回复。规则把步骤目标设为「事件来源」时调用的就是它。
- 提供动作 `send`，向指定会话主动发消息。
- 可选动作 `typing`：显示「正在输入」。
- 超过平台长度上限的文本由插件自己分段发送。

## Harness 约定

对话类动作插件（Agent、LLM）提供动作 `chat`：

| 参数 | 说明 |
|---|---|
| `prompt` | 必填。本轮输入 |
| `session_key` | 相同的 key 续接同一段对话；默认取触发事件的 `conversation.id`，没有事件时每次都是新对话 |
| `system_prompt` | 可选 |
| `new_session` | 布尔。为真时丢弃该 key 之前的对话，重新开始 |

输出至少包含 `text`（回复文本）和 `session_id`。插件用 `state.get/set` 自行维护 `session_key → 会话` 的映射。

## 接入向导

`setup: true` 的插件在用户点「添加」时不显示配置表单，而是由 Cat 以 `mode: "setup"` 启动一个临时进程并调用 `setup.run`。插件通过 `setup.show` / `setup.ask` 展示步骤，全部由 Cat 的界面渲染，插件不需要写前端。`setup.run` 返回的 `config` 被保存为实例配置，随后实例正常启动。

步骤对象：

| 字段 | 说明 |
|---|---|
| `kind` | `info` / `progress` / `qrcode` / `link` / `form` / `confirm` |
| `title`、`description` | 标题与说明（说明里的换行会保留） |
| `url` | 任何步骤都可带，界面显示「打开链接」按钮 |
| `progress` | 0 到 1；负数表示进度未知 |
| `text` | `qrcode`：要编码成二维码的文本 |
| `image` | `qrcode`：直接显示的图片（`data:` URL），优先于 `text` |
| `schema`、`values` | `form`：表单的 JSON Schema 与初始值 |
| `danger` | `confirm`：确认按钮显示为危险操作 |

`setup.ask` 的回答：`form` 返回各字段的值；`confirm` 返回 `{ "confirmed": true | false }`。

向导期间的 `data_dir` 就是最终实例的目录，所以向导里产生的登录态等文件，实例启动后可以直接使用。重新运行已有实例的向导时，Cat 会先停止该实例；向导失败或取消时恢复原配置。

## 托管依赖

插件需要外部程序时，在 `plugin.json` 里声明，由 Cat 下载、校验、解压，插件不需要自己实现：

```jsonc
"dependencies": [{
  "name": "napcat",
  "title": "NapCat",
  "version": "4.8.0",
  "notice": "向用户展示的补充说明",
  "platforms": {                       // 键：os/arch（Go 的命名）、os 或 any
    "windows/amd64": {
      "url": "https://…/NapCat.zip",   // 必须是 HTTPS
      "sha256": "…",                   // 必填，64 位十六进制
      "format": "zip",                 // zip | tar.gz | file，省略时按 URL 后缀判断
      "strip": 0                       // 解压时去掉的前导目录层数
    }
  }
}]
```

插件调用 `host.dependency { name }` 得到安装目录。规则：

- 没有有效 SHA-256 的依赖不会被安装；校验不符的文件会被丢弃。
- 首次下载前 Cat 会向用户展示来源地址并征得同意。同意按「插件 + 依赖 + 版本 + 校验值」记录，升级版本会再次询问。
- 询问只能发生在接入向导里。运行期间需要下载而用户尚未同意时，`host.dependency` 返回错误，提示用户重新运行向导。因此请在 `setup.run` 里调用一次 `host.dependency`。
- 压缩包中的符号链接会被忽略，越出目标目录的路径会导致安装失败。

## 安全须知

- 插件以用户身份运行，没有沙箱，能做用户能做的任何事。只安装你信任的插件。
- 不要把密钥写进日志。配置里 `format: "password"` 的字段只是界面上遮挡显示。
- 处理 `ingress.request` 时自行校验调用方（签名或密钥）；Cat 不替插件做鉴权。
