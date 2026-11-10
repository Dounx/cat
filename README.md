# Cat 事件中心

把各处来的事件接到一起，按规则筛选，再触发动作。

```
QQ / Discord / Telegram / 企业微信 / RSS / 网页变动 / 接口变动 / WebHook ──▶ 规则（条件）──▶ 动作流水线
                                                                    ├─ 交给 Agent（Claude Code / Codex / DeepSeek）
                                                                    ├─ 原路回复、跨平台转发
                                                                    └─ 发 HTTP 请求、跑脚本
```

- **插件化**：每个平台、每种 Agent 都是独立进程的插件，用任何语言都能写。官方提供 Go 和 TypeScript SDK。
- **一键接入**：接入向导负责校验密钥、显示登录二维码、下载并配置依赖，配置表单由插件声明、界面自动生成。
- **可调试**：所有事件和每次规则执行的每一步都有记录，历史事件可以重放。
- **两种形态**：桌面应用（托盘常驻）和无界面服务，共用同一个引擎和界面。

## 运行

需要 Go 1.24+、Node + pnpm（构建界面）、Bun（构建 TypeScript 插件）。

```bash
make all                                 # 界面、插件、bin/catd
./bin/catd --plugins ./plugins           # 然后打开 http://127.0.0.1:7788
```

桌面应用（macOS）：

```bash
make app                                 # 生成 bin/Cat.app，插件已打包在内
```

`catd` 的主要参数：

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `--addr` | `CAT_ADDR` | 监听地址，默认 `127.0.0.1:7788` |
| `--data` | `CAT_DATA` | 数据目录，默认 `~/.cat` |
| `--token` | `CAT_TOKEN` | 访问令牌。监听非本机地址时必须设置 |
| `--plugins` | `CAT_PLUGINS` | 额外的插件目录 |
| `--market` | `CAT_MARKET` | 插件市场索引地址 |

## 第一条规则

1. 「接入」→ 添加 **WebHook**，复制它的地址。
2. 「规则」→ 新建：来源选这个 WebHook，条件 `payload.text` 包含 `deploy`，动作选「HTTP 请求」。
3. 发一个请求试试：

   ```bash
   curl -X POST http://127.0.0.1:7788/ingress/<实例 ID> -d '{"text":"please deploy"}'
   ```

4. 在「事件」和「运行记录」里看结果。

接入了聊天平台和 Agent 之后，「规则」页会出现「消息 → Agent → 回复」按钮，一键生成最常用的规则。

## 订阅 RSSHub / RSS

「接入」→「监控」→ 添加 **RSSHub / RSS**，在「订阅源」里每行填一个：

```
/github/issue/DIYgod/RSSHub          # RSSHub 路由，拼在「RSSHub 实例地址」后面
https://go.dev/blog/feed.atom        # 普通 RSS / Atom 源，填完整地址
```

每个新条目是一个 `feed.item` 事件：`payload.text` 是「标题 + 摘要 + 链接」，可以直接转发；另有 `title`、`link`、`content`（纯文本全文）、`content_html`、`author`、`categories`、`images`、`published`。`conversation.id` 是订阅源本身，`conversation.name` 是它的标题，可以用来区分不同的源。

新加入的订阅源只记录现有条目，之后出现的才会触发规则。公共实例 `rsshub.app` 经常被限流，建议填自建实例的地址。

## 监控网页变动

「接入」→「监控」→ 添加 **网页变动**，在「网页」里每行填一个地址；只想盯页面上的某一块时，在地址后面空一格写 CSS 选择器：

```
https://example.com/changelog
https://example.com/pricing  #price    # 只看 id 为 price 的元素
```

比较的是页面上看得见的文字，标签、样式和空白的变化不算。文字变了就发出一个 `page.changed` 事件：`payload.text` 是「标题 + 新增和删除的行 + 地址」，可以直接转发；另有 `url`、`title`、`added`、`removed`（数组）、`content`（现在的全文）、`previous`（上一次的全文）。

插件只负责发现变化。要收到通知，建一条规则：来源选这个监控，动作选聊天渠道的「发送到指定会话」，内容填 `{{ payload.text }}`。

- 第一次看到的网页只记录现状，之后变了才触发。
- 每次打开都不一样的内容（时间、访问量）会造成误报：用选择器缩小范围，或在「忽略的内容」里写正则把它们去掉。
- 只抓取网页源码，不执行 JavaScript；靠脚本渲染的页面抓不到内容，可以用下面的「接口变动」改盯它背后的数据接口。

## 监控接口返回变动

「接入」→「监控」→ 添加 **接口变动**，在「接口」里每行填一个地址；只想盯返回里的某个字段时，在地址后面空一格写字段路径；不是 GET 请求时把方法写在最前面：

```
https://api.example.com/app/latest
https://api.example.com/app/latest  data.version     # 只看这个字段
POST https://api.example.com/orders/query  data.items  # 请求体填在「请求体」里
```

比较的是解析后的 JSON，字段顺序和空白的变化不算。数据变了就发出一个 `api.changed` 事件：`payload.text` 是「接口 + 变了的字段」，可以直接转发；另有 `changes`（数组，每项有 `path`、`type`（`added` / `removed` / `changed`）、`old`、`new`）、`value`（现在的值）、`previous`（上一次的值）、`url`、`method`、`path`。返回不是 JSON 时整段当作一个字符串比较。

和网页变动一样，插件只负责发现变化，通知靠规则：来源选这个监控，动作选聊天渠道的「发送到指定会话」，内容填 `{{ payload.text }}`。

- 第一次请求只记录现状，之后变了才触发。
- 每次请求都不一样的字段（时间戳、请求 ID）会造成误报：用字段路径缩小范围，或在「忽略的字段」里把它们去掉，例如 `timestamp`、`items[*].updated_at`。
- 接口报错（状态码不是 2xx），或返回里找不到要盯的字段，算检查失败，不算变动。
- 认证信息填在「Authorization」里，会按密钥保存；其他请求头填在「请求头」。它们对同一个监控里的所有接口生效，认证不同的接口另建一个监控。
- 列表里插入或删除一项时，只报告多出来或少掉的那一项，不会把后面的每一项都算作变了。

## 规则怎么写

条件是 [expr](https://expr-lang.org) 表达式，界面里可以用可视化条件构造器，也可以直接写：

```
payload.mentioned == true and actor.id in ["123", "456"]
```

可用变量：`payload`、`actor`、`conversation`、`type`、`source`、`raw`。

动作参数里用 `{{ }}` 取值：`{{ payload.text }}` 是事件内容，`{{ steps.agent.text }}` 是标识为 `agent` 的那一步的输出。

## 已有的插件

| 插件 | 说明 |
|---|---|
| WebHook（内置） | 任意 HTTP 请求变成事件，支持密钥和 HMAC 签名 |
| HTTP 请求、运行脚本（内置） | 动作 |
| Telegram | Bot，长轮询 |
| Discord | Bot，Gateway |
| QQ（OneBot） | OneBot 11。Windows 上可自动下载配置 NapCat；其他系统连接你自己运行的 OneBot 服务 |
| 企业微信 | 自建应用的消息回调 |
| RSSHub / RSS | 订阅 RSSHub 路由和普通 RSS / Atom 源，有新条目时发出 `feed.item` 事件 |
| 网页变动 | 定时检查网页，文字内容有变化时发出 `page.changed` 事件，可用 CSS 选择器只盯一块 |
| 接口变动 | 定时请求 HTTP 接口，返回的数据有变化时发出 `api.changed` 事件并列出变了的字段，可用字段路径只盯一处 |
| Claude Code / Codex / DeepSeek | Agent，按会话续接对话 |
| 大模型对话 | 任意 OpenAI 兼容接口，纯对话 |

## 安全

- **聊天消息会原样成为给 Agent 的指令。** 所以 Agent 插件默认只读、只接受白名单里的发送者。放宽之前请想清楚谁能给你的机器人发消息。
- **插件没有沙箱**，以你的用户身份运行。只安装你信任的插件。从市场安装时会校验 SHA-256。
- 默认只监听本机。要对外提供服务必须设置 `--token`；WebHook 请配置密钥。
- 密钥类配置存放在系统钥匙串（桌面版）或 `~/.cat/secrets.json`（无界面版，仅靠文件权限保护，未加密）。

## 开发

```bash
make test                                # 全部测试
go run ./cmd/catd --plugins ./plugins    # 后端
cd frontend && pnpm dev                  # 界面热更新（代理到 7788）
```

### 发布

推送和 PR 会跑 CI（`.github/workflows/ci.yml`）。推一个 `v` 开头的标签就会发布：

```bash
git tag v0.2.0 && git push origin v0.2.0
```

发布流程（`.github/workflows/release.yml`）先跑一遍 CI，再把这些放进 GitHub Release：

- `cat-<版本>-<系统>-<架构>.tar.gz`（Windows 是 `.zip`）：`catd` 和全部官方插件，解压即可运行；`SHA256SUMS` 是它们的校验和。
- `Cat-<版本>-macos-arm64.zip`：桌面应用。没有 Apple 开发者签名，首次打开需要在「系统设置 → 隐私与安全性」里放行。
- 插件市场：每个插件每个平台一个 zip，加上 `index.json`。把 `--market` 指向 `https://github.com/<用户>/<仓库>/releases/latest/download/index.json` 就能一直用最新一版的市场。

在 Actions 页面手动运行 Release 只构建、不发布，产物留在这次运行的附件里，可以用来试跑。本地也能打同样的包：

```bash
make frontend
python3 scripts/build-market.py <基础地址>   # 输出到 dist/market
python3 scripts/build-release.py v0.2.0      # 输出到 dist/release
```

- [写一个插件](docs/plugin-development.md)
- [插件协议](docs/plugin-protocol.md)

目录：`core/` 引擎，`app/` 组装，`cmd/catd` 无界面外壳，`desktop/` 桌面外壳（独立 Go 模块），`frontend/` 界面，`sdk/` 插件 SDK，`plugins/` 官方插件，`templates/` 插件模板。
