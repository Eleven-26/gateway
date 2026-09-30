# gateway 项目约定（Agent 工作指令）

> **本文件的来源**：`.workbuddy/memory/MEMORY.md`（workbuddy 长期记忆，2026-09-30 重构确立）。
> 这里把其中的**工程约定、关键常量、运行口径、生产红线**固化为仓库级指令，便于 agent 自动生效；
> 原文件仍是完整记忆（含每日流水 `2026-09-30.md`），两者冲突时以本文件 + 实际代码为准，并同步回改记忆文件。
>
> 项目定位：项目名 **`gateway`**（module `gwlab`），go 1.26.5，Windows 本机开发。一台**手写教学型 API 网关**
> （七级流水线：路由 / 鉴权 / 限流 / 熔断 / 选节点 / 转换转发 / 可观测），
> 唯一第三方依赖 `google.golang.org/grpc`，其余全用标准库。**不是 gin**。

---

## 1. 工程约定（硬约束）

1. **标准 Go 布局**：可执行入口在 `cmd/{gateway,backend,hashring}`；实现全部在 `internal/` 下按机制分包：
   `config / router / auth / ratelimit / breaker / balancer / transcode / proxy / observability / gateway`。
2. **仓库内不放编译产物**：`bin/`、`*.exe`、`*.exe~`、`*.log` 均已进 `.gitignore`。产物统一 `make build` 到 `bin/`。
3. **依赖方向单向**，改代码时不要引入反向依赖：

   ```
   config（叶子）→ router / auth / ratelimit / breaker / balancer / transcode
                 → proxy（额外依赖 auth 取身份、observability 取 TraceID 头名）
                 → gateway（唯一装配层，只被 cmd/gateway 引用）
   ```
4. **新增包必须写 `// Package xxx 实现 ① …` 形式的包注释**，并保持原项目的说明风格：
   写清「为什么值得做」「容易做错的地方」「本机实测数据」。
5. **新增路由 / 上游只改 `internal/config/config.go` 的 `defaultRoutes()` / `defaultServices()`**
   —— 配置是 Go 字面量，不是外部文件；敏感值在 `Default()` 里。
6. **熔断器 / 均衡器 / 反向代理都是按需惰性创建并缓存在 `gateway.Gateway` 的 map 里**；
   均衡器（含 `least_conn` 的节点计数）由 `balancer.New` 自己完成初始化，不要在外部预置。
7. **均衡器有单元测试** `internal/balancer/balancer_test.go`（Pick/Done 配对、并发配对、
   平局公平性、工厂按 `Balance` 分发）；**改 `least_conn` 或新增均衡算法后必须跑 `make test`**
   —— 在途计数只增不减这类错误串行请求看不出来，只能靠单测 + 并发/A-B 实测兜住。

## 2. 关键常量

- 网关监听 `127.0.0.1:18080`。
- 演示后端 HTTP `19001 / 19002 / 19003`，gRPC `19100`（只有 A 实例开 gRPC）。
- JWT 密钥 `demo-secret-do-not-use-in-prod`，API Key `ak_live_9f2c41d7`（均在 `internal/config/config.go`）。
- 管理端点 `/metrics`、`/debug/logs` 不经过路由与鉴权。
- ④ 演示服务 `slow-svc`（`least_conn`，19001/19002/19003）+ `/slow` 路由；
  演示后端支持 `?ms=<毫秒>` 人为延时（**上限 5s**，见 `cmd/backend`）——用它占住连接，
  否则 `least_conn` 看不出差别（串行请求在途恒为 0，退化成平局轮转）。

## 3. 本机运行注意事项（实测踩过）

- **长驻进程要「分离」启动**：`Start-Process -FilePath bin\gateway -RedirectStandardOutput bin\gw.log`。
  本轮实测用后台任务包装器启动 `bin/gateway` / `bin/backend` 时，任务立刻返回「exit 0」而进程不留
  （表现为 curl 全部 connection refused）；`Start-Process -PassThru` 起得来、`HasExited=False`。
- **沙箱受限时任何命令都起不来**（`pwsh` 退出码 `0xC0000142`），这不是文件权限问题：
  ACL 诊断脚本判定 `NOT_THIS_CLASS`、未改动任何权限（报告在 `D:\www\gateway-acl-report\`）；
  把会话切到完全权限即恢复正常。
- **读写 Go 源码不要用 `Get-Content`**：PowerShell 5.1 按 ANSI 解码 UTF-8，会把中文与换行读坏
  （行数统计偏小、`-replace` 静默失效）；用 `[System.IO.File]::ReadAllText($p, [System.Text.Encoding]::UTF8)`。
- **限流验证不能用 shell 的 `for curl` 循环**：Windows 下每次 curl 启动约 200ms，
  实际速率低于阈值打不出 429；用 Python `urllib` + `threading` 才测得准。
- curl 一律加 `--noproxy '*'`；Python 用 `urllib.request.ProxyHandler({})`。
- `go build -o bin/gateway` 产物名就是 `bin/gateway`（`-o` 指定精确名，不自动补 `.exe`），
  因此**不要**用 `./bin/gateway.exe` 去启动。
- `go vet ./...` 前台执行偶发 SIGTERM，长命令统一后台跑。

## 4. 生产口径与上线红线

- **生产只运行 `cmd/gateway`**。`cmd/backend` 是演示替身（生产里被真实业务服务顶替，语言不限）、
  `cmd/hashring` 是离线测量工具，两者都不进生产环境。
- **接入新后端服务只改 `internal/config/config.go` 一个文件**：加一个 `Service` + 一条 `Route`
  → `make build` → 重启。路由与服务解耦（多条路由可指向同一服务），不涉及网关逻辑代码改动。
- 配置是**编译期硬编码**（`cmd/gateway/main.go` 的 `config.Default()`），
  无 flag / 无配置文件 / 无配置中心 —— 这是当前最主要的「生产化缺口」。
- 上线前必做三件事：①密钥改环境变量注入（现为源码明文）
  ②**确认没有 `prefix /` 的兜底路由**（演示配置里的 `fallback` 已删；生产接入时不要加回
  ——否则前端拼错路径静默打到 `order-svc`，404 `route_not_found` 永不触发）
  ③给 `/metrics`、`/debug/logs` 加访问控制（它们在 `ServeHTTP` 里排在路由与鉴权**之前**）。
- 热重载的两个坑（改配置加载方式时必看）：①`gateway.Gateway` 的
  `breakers`/`balancers` 以**服务名**为 key、`proxies` 以 `name|addr|strip` 为 key，
  同名服务改地址会留下永不释放的旧 `proxy.Proxy`；②若热重载是整体替换 `Gateway`，
  必须调用旧实例 `Close()`，否则 gRPC 连接池泄漏；③熔断器状态不能重建
  （刚打开的熔断器会被重置回 closed，把流量又送给未恢复的上游）。
- `Route.Upstream` 指向不存在的服务名 → 运行期 **500 `bad_config`**（`internal/gateway/gateway.go`），
  消息里带未定义的服务名。

## 5. 已知缺口：先对齐预期，再动手

以下均为源码 / 实测确认的问题，**不要当成 bug 顺手"修好"而不说明**；要么按计划修并更新文档，要么保持现状：

- `proxy` 的上游错误不会落进访问日志的 `err=` 字段（`ErrorHandler` 收到的是 `req.Clone` 深拷贝的出站请求，Header 改不回来）。
- `order-svc` 配了权重 `3:1:1`，但算法是 `round_robin`，权重不生效。
- 入站 `X-Forwarded-For` 不清理可伪造；取真实 IP 必须用 `X-Real-IP`。
- `BreakerConfig` 的 `FailRatio` / `MinRequests` 零值不兜底：`FailRatio: 0` 意味着**一次失败即熔断**。
- 限流桶、熔断器、指标全在单进程内存；多副本部署需外部化（限流阈值会变成 N 倍）。
- `/metrics` 不暴露每个节点的在途计数，`least_conn` 的计数不变式只能靠单元测试
  （`internal/balancer/balancer_test.go`）与 A/B 实测验证，没有指标可观测。

> **已修（记录在此以免回退）**：`least_conn` 的平局偏置 —— 原实现按 map 迭代顺序取第一个最小值
> （`for addr, c := range b.conns`，Go 小 map 迭代起点随机但首元素占优），实测全 0 平局时
> 78% 集中到列表首节点（a:1=777 / b:1=116 / c:1=107，全 0 平局）；现按轮转起点取最小、平局轮流
> （999 次 = 333/333/333）。

## 6. 文档同步要求

- `docs/项目地图.md`：文件 / 依赖 / 请求链路 / 配置 / 改动定位的行号级索引
  （§3 链路、§4 配置、§6 缺口；改动后同步这三节）。
- `docs/项目分析与执行链路.md`：完整分析与执行链路，含实测输出与已知问题清单（§8）。
- `docs/网关缺陷审计与优化路线.md`：独立审计（P0/P1/P2 缺陷 + 对标标准网关的能力矩阵 + 四批路线图）。
  **修掉其中任一条后，必须回来把它从缺陷清单移入"已验证的正面结论"或标注已修**，避免审计文档过期。
- `docs/生产部署与接入新服务.md`：生产部署形态、接入新服务示例、配置取值速查、生产化改造清单、上线检查清单。
- `docs/images/网关请求执行链路.png`：七级流水线示意图。
- `docs/images/生产部署拓扑与入口职责.png`：生产部署拓扑。
- `docs/images/网关模块依赖与请求链路.png`：分层依赖 + 请求链路 + 实测数据的一页图
  （2026-09-30 新增，手绘于 Pillow；内容随本文 §1/§2/§4 与项目地图 §3/§4 同步）。
- **改动请求链路或配置模型后，必须同步更新上面几份文档里对应的章节。**
- 可视化组件落盘到 `docs/` 时**文件名会被规范化**（传入的标题前缀会被去掉，
  实际文件名为 `xxx.png`）。写文档引用前先列目录核对真实文件名，不要照抄标题。

## 7. 完整记忆索引

| 文件 | 内容 |
|---|---|
| `.workbuddy/memory/MEMORY.md` | workbuddy 长期记忆（本文件的来源） |
| `.workbuddy/memory/2026-09-30.md` | 当日流水：项目分析 / 标准工程化重构 / 生产部署文档 / 生产就绪度评估（含 P0/P1/P2 与 panic 行为实测） |
