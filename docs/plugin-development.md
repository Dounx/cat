# 写一个 Cat 插件

插件是一个独立的可执行程序，用任何语言都可以写。这份指南用官方 SDK 带你做出第一个插件；协议细节见 [plugin-protocol.md](plugin-protocol.md)。

## 十分钟上手

1. 复制一个模板：Go 用 [`templates/plugin-go`](../templates/plugin-go)，TypeScript 用 [`templates/plugin-node`](../templates/plugin-node)。
2. 改 `plugin.json` 里的 `id`、`name`，以及 `command`（要和你构建出的可执行文件名一致）。
3. 构建：

   ```bash
   go build -o cat-plugin-hello .
   ```

   TypeScript 模板开发期间不需要构建，`plugin.json` 直接用 `bun run src/index.ts` 运行。
4. 让 Cat 加载它。`--plugins` 指向**包含插件目录的那一层**：

   ```bash
   go run ./cmd/catd --plugins ./templates
   ```

5. 打开界面的「接入」页，你的插件会出现在「添加」里。改了代码后重新构建，再在实例上点「重启」。

## 一个插件由什么组成

| 部分 | 作用 |
|---|---|
| `plugin.json` | 声明名称、配置表单、发出的事件、提供的动作 |
| `Start` | 每个实例启动时调用一次：连接平台，开始发事件 |
| `Actions` | 规则里的动作节点调用它们；返回的对象就是这个动作的输出 |
| `Ingress`（可选） | 接收发到 `/ingress/<实例 ID>` 的 HTTP 请求 |
| `Setup`（可选） | 接入向导，用于需要引导、扫码或下载依赖的场景 |

配置表单、向导界面都由 Cat 渲染，你不需要写任何前端。

## 发事件

```go
h.Emit(cat.Event{
    Type:         "message.created",
    Conversation: &cat.Conversation{ID: "room-1", Kind: "group"},
    Actor:        &cat.Actor{ID: "42", Name: "Ann"},
    Payload:      map[string]any{"text": "hello", "message_id": "7"},
    Raw:          original, // 平台原始数据，原样保留，规则里用 raw.xxx 访问
})
```

`id`、`source`、`plugin` 由 Cat 填写。聊天类插件请遵守[聊天平台约定](plugin-protocol.md#聊天平台约定)，这样用户为一个平台写的规则换个平台也能用。

### 让规则说要盯什么

监控类插件盯什么（哪个订阅源、哪个页面）最好由规则来说，而不是让用户先在接入的配置里填一遍。在 `plugin.json` 里加 `trigger_params`，用户在规则的触发节点里选中这个接入时就会看到这份表单：

```json
"trigger_params": {
  "type": "object",
  "properties": { "feeds": { "type": "string", "title": "订阅源", "format": "textarea" } }
}
```

启用的规则填了什么，`Start` 里用 `h.Triggers()` 就能读到；之后规则有变化时 `Triggers` 会被调用，拿到新的完整列表。发事件时把触发的 `ID` 写进 `Triggers`，提出它的规则就只收到属于自己的事件：

```go
cat.Plugin{
    Start: func(ctx context.Context, h *cat.Host) error {
        watch(h.Triggers()) // 可能是空的：还没有规则用到这个接入
        return nil
    },
    Triggers: func(ctx context.Context, h *cat.Host, list []cat.Trigger) map[string]string {
        watch(list)
        return nil // 或 { 触发的 ID: "哪里不对" }，会显示在那条规则上
    },
}

h.Emit(cat.Event{Type: "feed.item", Triggers: []string{t.ID}, Payload: …})
```

`Triggers` 第一次被调用是在 `Start` 之前。同样的参数总是同一个 `ID`，细节见[协议文档](plugin-protocol.md#触发参数)。测试里用 `h.SetTriggers(…)` 模拟。

## 做动作

动作收到规则里填的参数（模板已经求值），以及触发规则的那个事件：

```go
"reply": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
    conv := actx.Event.Conversation.ID // 原路回复
    ...
    return map[string]any{"message_id": id}, nil
}
```

- 返回一个对象。规则里排在后面的节点用 `{{ steps.<节点标识>.<字段> }}` 引用它。输出有哪些字段不需要声明：动作跑过一次之后，编辑器的「插入变量」会按最近一次的输出列出来。所以字段名请保持稳定，没有值时也尽量带上这个字段。
- 返回错误会让这一步失败；同时返回的对象会作为部分输出记入运行记录。
- `ctx` 在动作超时或被取消时结束，请把它传给你的网络请求和子进程。
- 在 `plugin.json` 里把每个参数都写进 `params`，界面才会为它生成输入框。

### 替条件节点做判断

规则的条件节点可以不按字段比，而是交给一个接入来判断。插件声明一个名为 `judge` 的动作，它的接入就会出现在条件节点「模型判断」的列表里。

这个动作除了自己在 `params` 里声明的参数（界面按它们生成表单，`choices` 除外），还会收到 `choices`：一个字符串数组，是可以选的全部结果。条件只问是否时，它是 `["yes", "no"]`。动作返回的对象里必须有 `choice`，值是这些选项中的一个：

```go
"judge": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
    choices, _ := p["choices"].([]any)
    ...
    return map[string]any{"choice": picked}, nil
}
```

返回的 `choice` 不在选项里时，规则走「其他」那个出口；没有 `choice` 或返回错误，这条线到此为止并记为出错。事件每走到这个节点一次就调用一次，请不要在这里保留会话上下文。

## 保存状态

`h.StateGet` / `h.StateSet` 是按实例隔离的持久键值存储，适合放游标、会话映射这类小数据。大文件放 `h.DataDir`。两者都在实例被删除时清除。

## 报告连接状态

`Start` 返回错误表示启动失败（例如密钥无效），Cat 不会重试，等用户改配置。`Start` 成功之后连接断了，用 `h.SetStatus("error", "原因")` 告知，恢复后 `h.SetStatus("running", "")`，重连由插件自己负责。

## 接入向导

当「填一个表单」不够用时再写向导，比如要校验密钥、要扫码、要下载依赖：

```go
Setup: func(ctx context.Context, s *cat.Setup) (map[string]any, error) {
    out, err := s.Ask(ctx, cat.Step{Kind: cat.StepForm, Title: "连接", Schema: schema})
    if err != nil {
        return nil, err // 用户关闭向导时是 cat.ErrCancelled，原样返回即可
    }
    s.Progress("正在验证…", -1)
    ...
    s.Show(cat.Step{Kind: cat.StepQRCode, Title: "扫码登录", Text: loginURL})
    ...
    return map[string]any{"token": out["token"]}, nil // 成为实例配置
}
```

校验失败时不要直接返回错误，把原因写进说明再问一次，用户体验好得多（参考 [`plugins/telegram`](../plugins/telegram/telegram.go) 的 `setup`）。

需要外部程序时在 `plugin.json` 的 `dependencies` 里声明，调用 `s.Dependency(ctx, "名字")`，Cat 会征得用户同意后下载、校验、解压并返回目录（参考 [`plugins/onebot`](../plugins/onebot)）。

## 测试

Go SDK 带一个假宿主，不用启动 Cat 就能测：

```go
h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"token": "x"}})
if err := h.Start(); err != nil { t.Fatal(err) }
ev := h.NextEvent()
out, err := h.Invoke("reply", map[string]any{"text": "hi"}, &ev)
```

对接外部平台的插件，推荐像官方插件那样把 API 地址做成可配置项，测试时指向 `httptest` 起的假服务。

## 凭据

把密钥字段标成 `"format": "password"`。Cat 会把它存进系统钥匙串（桌面版）或权限受限的文件（无界面版），不写入数据库，界面和 API 里也只显示掩码。不要把密钥写进日志或错误信息。

## 发布

1. 为各平台构建可执行文件。TypeScript 插件用 `bun build --compile` 生成单文件，用户就不需要安装 Node。
2. 每个平台打一个 zip，根目录放 `plugin.json` 和可执行文件。
3. 在插件市场的索引里加一条，写明每个平台的下载地址和 SHA-256：

   ```json
   {
     "id": "hello", "name": "Hello", "version": "0.1.0", "description": "…",
     "platforms": {
       "darwin/arm64": { "url": "https://…/hello-0.1.0-darwin-arm64.zip", "sha256": "…", "format": "zip" }
     }
   }
   ```

`python3 scripts/build-market.py <基础地址>` 会为本仓库的全部插件完成这三步，输出到 `dist/market`。

## 安全提醒

插件以用户身份运行，没有沙箱。如果你的插件让外部输入驱动有副作用的操作（执行命令、调用 Agent），请像官方 Agent 插件那样默认拒绝、要求白名单，并在说明里写清风险。
