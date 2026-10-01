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

1. **标准 Go 布局**：可执行入口在 `cmd/{gateway,backend,hashring,statestore}`（`statestore` 是限流 / 熔断
   共享状态的**参考服务**，审计 C4；生产可换成自己的实现）；实现全部在 `internal/` 下按机制分包：
   `config / router / auth / ratelimit / breaker / overload / balancer / clientip / transcode / proxy / observability / gateway`
   （`overload` 是批次 C6 新增的负载保护包）。
   仓库另有 `scripts/`（压测与演练脚本：`loadtest.py` 纯标准库压测、`demo_faults.py` 故障演练、
   `demo_reload.py` 热重载验收）、`deploy/`（`docker-compose.yml`（含 `statestore`）、可直接加载的
   `config.example.json`、`k8s/{gateway,statestore}.yaml`、`observability/{prometheus.yml,grafana-dashboard.json}`）、
   根目录 `Dockerfile`（多阶段构建）与
   `.github/workflows/`（CI：gofmt 卡口 + vet + `-race` + 三入口构建 + 基准记录）。
2. **仓库内不放编译产物**：`bin/`、`*.exe`、`*.exe~`、`*.log` 均已进 `.gitignore`。产物统一 `make build` 到 `bin/`。
3. **依赖方向单向**，改代码时不要引入反向依赖：

   ```
   config（叶子）/ clientip（叶子）→ router / auth / ratelimit / breaker / balancer / transcode
                 → proxy（额外依赖 auth 取身份、observability 取 TraceID 头名）
                 → gateway（唯一装配层，只被 cmd/gateway 引用）
   ```
4. **新增包必须写 `// Package xxx 实现 ① …` 形式的包注释**，并保持原项目的说明风格：
   写清「为什么值得做」「容易做错的地方」「本机实测数据」。
5. **新增路由 / 上游只改 `internal/config/config.go` 的 `defaultRoutes()` / `defaultServices()`**
   —— 配置是 Go 字面量，不是外部文件；敏感值在 `Default()` 里。
6. **熔断器 / 均衡器 / 反向代理都是按需惰性创建并缓存在 `gateway.Gateway` 的 map 里**；
   均衡器（含 `least_conn` 的节点计数）由 `balancer.New` 自己完成初始化，不要在外部预置。
7. **12 个包都有用例**（auth / balancer / breaker / clientip / config / gateway / observability /
   overload / proxy / ratelimit / router / transcode），
   其中均衡器测试在 `internal/balancer/balancer_test.go`（Pick/Done 配对、并发配对、平局公平性、
   工厂按 `Balance` 分发、节点级摘除与 fail-open、节点级指标）；
   **改任一处都要跑 `make test`** —— 在途计数只增不减这类错误串行请求看不出来，
   只能靠单测 + 并发/A-B 实测兜住。

## 2. 关键常量

- 网关监听 `127.0.0.1:18080`。
- 演示后端 HTTP `19001 / 19002 / 19003`，gRPC `19100`（只有 A 实例开 gRPC）。
- JWT 密钥 `demo-secret-do-not-use-in-prod`，API Key `ak_live_9f2c41d7`（均在 `internal/config/config.go`）。
- 管理端点**独立监听 `127.0.0.1:18081`**（`Config.AdminListenAddr`，审计 P1-6）：`/metrics`、`/debug/logs`、
  `/readyz` 都在这个端口上、都不经过路由与鉴权；**业务端口 `18080` 不提供**（实测 404），
  只有 `AdminListenAddr` 留空时才退回「与业务同端口」。`/readyz` 的语义：任一**已被使用过**的服务处于
  熔断、或该服务所有节点被摘除 → 503，否则 200 并列出每个服务的 `breaker/upstreams/ejected`
  （`/healthz` 只代表进程活着）。
- **`Config.TrustedProxies []string`**：可信代理 CIDR（如 `10.0.0.0/8`）。只有直连对端本身是可信代理时
  才采信 `X-Forwarded-For`（从右往左跳过可信跳）或 `X-Real-IP`，否则**一个字节都不读**；
  空（`nil`）= 不信任任何 XFF，最安全 —— 演示配置就是 `nil`。
- **`Service.Ejection{FailThreshold, Cooldown}`**：节点级被动摘除（`internal/balancer/health.go`）。
  演示配置给 `order-svc` / `user-svc` / `slow-svc` 配了 `{FailThreshold: 3, Cooldown: 5s}`；
  `FailThreshold=0` 表示**不启用摘除**（`flaky-svc` 故意不配，那边演示服务级熔断），`Cooldown` 必须 > 0。
- **节点级指标**（`/metrics`，服务名排序输出）：`gw_upstream_inflight{service,addr}`、
  `gw_upstream_failures_total{service,addr}`、`gw_upstream_ejected{service,addr}`。
- ④ 演示服务 `slow-svc`（`least_conn`，19001/19002/19003）+ `/slow` 路由；
  演示后端支持 `?ms=<毫秒>` 人为延时（**上限 5s**，见 `cmd/backend`）——用它占住连接，
  否则 `least_conn` 看不出差别（串行请求在途恒为 0，退化成平局轮转）。
- **请求体上限 `maxBodyBytes = 8MB`**（`internal/gateway/gateway.go`）：路由匹配后、鉴权前按
  `Content-Length` 直接拒绝，并用 `MaxBytesReader` 兜底超限读取；超限 → **413**
  （由 `internal/proxy/proxy.go` 的 `proxy.ErrorHandler` 映射）。
- **`http.Server` 读写超时**（`cmd/gateway/main.go`）：`ReadTimeout 30s`、
  `WriteTimeout = 配置里最大 Service.Timeout + 15s`（新 API `config.MaxServiceTimeout()`，
  `internal/config/config.go`）。⚠️ `WriteTimeout` 会掐断超过该时长的长连接流式响应（SSE）；
  要长流需按路由放宽或用 `http.ResponseController.SetWriteDeadline` 续期。
- **JWT 必须带 `exp` 且寿命 ≤24h**（`internal/auth/auth.go` 的 `maxTokenLifetime = 24h`）：
  `exp == 0` 直接拒（401 `token_no_expiry`）；`iat` 存在时校验 `exp-iat ≤ 24h`
  （超长 → `token_too_long_lived`）；`SignJWT` 自动补 `iat`。
- **`GW_CONFIG` 与外部配置**（审计 C1，`internal/config/file.go`）：`GW_CONFIG` 指向一份 JSON，
  与内置默认值（`config.Default()`）合成——**文件是"覆盖"而不是"合并"**：
  - `routes` / `services` 一写就**整体替换**（避免"删不掉的旧服务"）；
  - `DisallowUnknownFields`：字段名拼错（`timeout` 写成 `time_out`）**启动失败**，不静默忽略；
  - 时长一律写字符串：`"2s"` / `"500ms"`（整数纳秒也接受）；
  - **下划线开头的 key 递归当注释忽略**（`_说明` / `_comment` 都行）；标量/子结构用指针区分
    「没写」与「显式零值」；**容忍 Windows 编辑器与 `Set-Content -Encoding UTF8` 的 UTF-8 BOM**；
  - `deploy/config.example.json` 是可直接加载的示例（含注释约定示范与 TLS 段）。
- **环境变量优先级最高**（环境变量 > 文件 > 内置默认）：`GW_LISTEN_ADDR`、`GW_ADMIN_LISTEN_ADDR`、
  `GW_JWT_SECRET`、`GW_API_KEY`、`GW_TRUSTED_PROXIES`（分隔符 `,` `;` 空格均可）。
  启动横幅打印**配置来源**（`built-in（编译期默认值）` / 文件路径 / `+ 环境变量覆盖`），
  配置校验不过 → **启动失败**。
- **热重载**：`cmd/gateway` 每 **3 秒**轮询 `GW_CONFIG` 文件的 mtime（Windows 没有 SIGHUP，轮询在两个
  平台行为一致），变了就 `config.Load()` + `Gateway.Reload()`（`internal/gateway/reload.go`）——
  **熔断器按服务名复用、绝不重建**（重建会把刚打开的熔断器重置回 closed）、均衡器只在
  「算法 / 节点列表 / 摘除参数」变化时重建、代理缓存按新配置清理（key 为 `service|addr|strip`）、
  任何一步失败都**保留旧配置**。日志形如
  `配置已热重载: routes=2 services=2 熔断器复用=2 均衡器重建=[hot-svc] 拓扑未变=[self] 服务删除=[] 代理清理=1`。
- **`SharedState{url, timeout, fail_open, sync_interval}`**（审计 C4）：限流判定走 `POST /ratelimit`，
  熔断状态走 `POST /breaker`（发布本地跳闸）+ `GET /breaker`（周期拉取别人的跳闸）；
  参考实现 `go run ./cmd/statestore -listen 127.0.0.1:18090`。读不到时**降级**而不是失败：
  限流按 `fail_open` 放行、熔断退回本地状态机，两者都计入
  **`gw_shared_state_errors_total{backend=...}`**。熔断是**周期性同步**（`sync_interval` 默认 1s），
  副本之间生效有一个时间窗；`timeout` 默认 200ms。
- **`Overload{max_inflight, max_queue, queue_timeout, retry_after}`**（审计 C6，`internal/overload`）：
  `max_inflight=0` 表示不启用；并发满时有界排队，排队满/超时 → **503** + `Retry-After` +
  `X-Load-Shed: queue_full | queue_timeout`；指标 `gw_overload_rejected_total{route}`、
  `gw_inflight_limit`、`gw_queue_waiting`。闸门位置：**`/healthz` 等 self 路由之后、鉴权之前**
  （`internal/gateway/gateway.go` 的「负载保护」段）—— 负载高时把健康检查也拒了，LB 会摘实例、
  剩下的实例压力更大，反而加速雪崩。**与限流的区别**：限流管速率（谁在用），负载保护管并发（此刻多少在飞）。
- **上游 TLS**（审计 C2，`internal/config/tls.go`）：`Upstream.scheme`（`http` / `https`）+
  `Upstream.tls{ca_file, server_name, insecure_skip_verify, client_cert_file, client_key_file}`；
  HTTP 代理与 gRPC 转码**共用同一份 TLS 语义**（改一处两处都变）。证书文件在**启动期**读取并解析，
  路径写错 / 证书私钥不匹配 → **启动失败**；**绝不静默降级**。`server_name`（SNI）在用 IP 直连而证书
  签的是域名时**必须**显式给，否则握手因主机名不匹配失败。
- **每路由超时 + 重试**（审计 C3）：`Route.timeout` 覆盖 `Service.Timeout`；
  `Route.retry{attempts, per_try_timeout, allow_non_idempotent}`。只重试**未写出任何字节**的失败
  （拨号被拒/超时、TLS 握手失败），**5xx 不重试**，默认**只重试幂等方法**（GET/HEAD/PUT/DELETE/OPTIONS/TRACE）
  且**请求体必须为空**；`config.MaxServiceTimeout()` 已把重试算进 `http.Server.WriteTimeout`
  （否则会把还在重试的请求拦腰掐断）。
- **指标口径（审计 C5）**：`gw_request_duration_seconds_bucket / _sum / _count` —— 单位是**秒**，
  可跨实例聚合，分位用 Prometheus `histogram_quantile` 算；旧的 `gw_request_duration_ms{quantile}`
  保留但**多副本不要聚合它**。运行时指标 `gw_runtime_goroutines` / `gw_runtime_mem_alloc_bytes` /
  `gw_runtime_mem_sys_bytes` / `gw_runtime_gc_cycles`。`/metrics` 输出**已排序**（稳定，可逐字节 diff）。
- **W3C 链路上下文**（审计 C5，`internal/observability/trace.go`）：入站 `traceparent` **沿用上游
  trace-id**、**为本跳生成新 span-id**、`tracestate` 原样透传；非法/缺失时退回 `X-Trace-Id`
  （兼容保留）再新起一条链路。访问日志新增 **`span=`**（`internal/observability/observ.go`）。
  实测同一请求 trace-id 不变、span-id 变化（`ea436a45114500f5` → `35ed96d3ed632100`）。

## 3. 本机运行注意事项（实测踩过）

- **长驻进程要「分离」启动**：`Start-Process -FilePath bin\gateway -RedirectStandardOutput bin\gw.log`。
  本轮实测用后台任务包装器启动 `bin/gateway` / `bin/backend` 时，任务立刻返回「exit 0」而进程不留
  （表现为 curl 全部 connection refused）；`Start-Process -PassThru` 起得来、`HasExited=False`。
- **沙箱受限时任何命令都起不来**（`pwsh` 退出码 `0xC0000142`），这不是文件权限问题：
  ACL 诊断脚本判定 `NOT_THIS_CLASS`、未改动任何权限（报告在 `D:\www\gateway-acl-report\`）；
  把会话切到完全权限即恢复正常。
- **读写 Go 源码不要用 `Get-Content`**：PowerShell 5.1 按 ANSI 解码 UTF-8，会把中文与换行读坏
  （行数统计偏小、`-replace` 静默失效）；用 `[System.IO.File]::ReadAllText($p, [System.Text.Encoding]::UTF8)`。
- **PowerShell 写 JSON / YAML 配置时注意 UTF-8 BOM 与编码**：Windows PowerShell 5.1 的
  `Set-Content -Encoding UTF8` 会写入 **UTF-8 BOM**，而 `json.Decoder` 会把 BOM 当成第一个字符
  （报 `invalid character 'ï'`，从文件内容上看不出任何问题）。网关的配置加载器已**容忍并剥掉 BOM**
  （`config.OverlayJSON`），但这仍会影响：把同一份文件喂给 `docker-compose` / `kubectl`、
  或做逐字节对比时「内容看起来一样却对不上」。需要无 BOM 时显式指定（如 `[System.IO.File]::WriteAllText`
  配合 `UTF8Encoding($false)`），对比前先确认编码。
- **限流验证不能用 shell 的 `for curl` 循环**：Windows 下每次 curl 启动约 200ms，
  实际速率低于阈值打不出 429；用 Python `urllib` + `threading` 才测得准。
- curl 一律加 `--noproxy '*'`；Python 用 `urllib.request.ProxyHandler({})`。
- `go build -o bin/gateway` 产物名就是 `bin/gateway`（`-o` 指定精确名，不自动补 `.exe`），
  因此**不要**用 `./bin/gateway.exe` 去启动。
- `go vet ./...` 前台执行偶发 SIGTERM，长命令统一后台跑。
- **`go test -race` 在本机不可用**：`CGO_ENABLED=0` 且无 gcc，报 `-race requires cgo`；
  竞态检测只能放 CI（Linux）。

## 4. 生产口径与上线红线

- **生产只运行 `cmd/gateway`**。`cmd/backend` 是演示替身（生产里被真实业务服务顶替，语言不限）、
  `cmd/hashring` 是离线测量工具，两者都不进生产环境。
- **接入新后端服务只改 `internal/config/config.go` 一个文件**：加一个 `Service` + 一条 `Route`
  → `make build` → 重启。路由与服务解耦（多条路由可指向同一服务），不涉及网关逻辑代码改动。
- **配置 = 内置默认值（`config.Default()`）→ `GW_CONFIG` 指向的 JSON → 环境变量覆盖**（审计 C1）；
  `cmd/gateway` 启动即 `cfg.Validate()`，**配置写错就起不来**。仍未接**配置中心**
  （无 etcd / Nacos 之类的下发与版本化）—— 这是剩下最主要的生产化缺口。
- 上线前必做三件事：①密钥改环境变量注入（现为源码明文）
  ②**确认没有 `prefix /` 的兜底路由**（演示配置里的 `fallback` 已删；生产接入时不要加回
  ——否则前端拼错路径静默打到 `order-svc`，404 `route_not_found` 永不触发）
  ③**管理端口必须只对内网/回环开放**（`AdminListenAddr`，默认 `127.0.0.1:18081`；`/metrics`、
  `/debug/logs`、`/readyz` 已默认不在业务端口提供，只有 `AdminListenAddr` 留空才退回同端口）。
- **`TrustedProxies` 必须与真实 LB / CDN 网段一致**：留空（`nil`）表示不信任任何 XFF（最安全）；
  写错的后果是限流维度退化成 LB 地址 —— 所有匿名客户端共用一个桶，一个客户端就能把全网打成 429。
- **`Ejection` 阈值/冷却要按业务调**：太小会因网络抖动就把节点摘掉，冷却太长又白白损失容量；
  `FailThreshold=0` 表示不启用节点级摘除。
- **配置写错会「启动失败」**（`gateway.New` 第一件事就是 `cfg.Validate()`，`internal/config/validate.go`）
  —— 这是特性：错误配置在启动期暴露，而不是等某个请求打进来才变成运行期 500 `bad_config`。
- 热重载的两个坑（改配置加载方式时必看）：①`gateway.Gateway` 的
  `breakers`/`balancers` 以**服务名**为 key、`proxies` 以 `name|addr|strip` 为 key，
  同名服务改地址会留下永不释放的旧 `proxy.Proxy`；②若热重载是整体替换 `Gateway`，
  必须调用旧实例 `Close()`，否则 gRPC 连接池泄漏；③熔断器状态不能重建
  （刚打开的熔断器会被重置回 closed，把流量又送给未恢复的上游）。
- `Route.Upstream` 指向不存在的服务名 → 现在**启动期就被 `cfg.Validate()` 拒掉**（`internal/config/validate.go`）；
  `internal/gateway/gateway.go` 里那段运行期 **500 `bad_config`** 兜底仍在，但正常配置下到不了。
- **多副本必须配 `shared_state`**（审计 C4）：不配则**限流放行量 = 副本数 × 阈值**、
  A 副本已熔断的服务 B 副本还在打流量（单副本压测完全看不出来这两个问题）。
  配上之后要注意：状态服务本身成了**新的单点**（必须能快速重启或水平扩展）；
  熔断是**周期性拉取**（`sync_interval` 默认 1s），副本之间生效有一个时间窗；
  读不到时按降级语义走，并用 `gw_shared_state_errors_total{backend=...}` 告警。
- **`overload` 阈值必须按压测拐点定**（审计 C6）：用 `scripts/loadtest.py` 找出网关在不劣化前提下的
  并发拐点，取其 70% 左右；`max_queue` 只吸收突发，`queue_timeout` / `retry_after` 要大于上游典型耗时。
  闸门在 **self 路由之后、鉴权之前**，所以 **`/healthz` 不参与负载保护**（负载高时把健康检查也拒了，
  LB 会摘实例、加速雪崩）。
- **上游 TLS 证书在启动期校验**（审计 C2）：`Upstream.scheme=https` + `tls{...}`；路径写错或
  证书私钥不匹配**启动就失败**（不会留到第一个请求才报握手错）；`insecure_skip_verify` 只应临时调试用。
- **改 `SharedState.URL` 或 `ListenAddr` / `AdminListenAddr` 需要重启**：热重载只替换配置快照，
  不重开监听、也不重建共享状态客户端；其余配置项改完 3 秒内生效。
- **`/healthz` 接 liveness、`/readyz` 接 readiness**：`/healthz` 只代表进程活着（因此刻意**不参与**
  负载保护）；`/readyz` 才看上游 —— 任一**已被使用过**的服务处于熔断、或该服务所有节点被摘除 → 503。
  K8s 探针按这个分工配，否则上游全挂时 Pod 不会从 Service 摘除。

## 5. 已知缺口：先对齐预期，再动手

以下均为源码 / 实测确认的问题，**不要当成 bug 顺手"修好"而不说明**；要么按计划修并更新文档，要么保持现状：

- **出站 `X-Forwarded-For` 仍原样转发给上游**：本轮只修了**入站取值**（`internal/clientip` 只在直连对端本身是
  可信代理时才采信 XFF（从右往左跳过可信跳）或 `X-Real-IP`，`TrustedProxies` 为空时**一个字节都不读**）；
  出站方向没有清理/重写，客户端给的 XFF 会原样带给上游。
- **无主动健康探活**：只有**被动**摘除（`internal/balancer/health.go` 的 `Ejection`）与服务级熔断，
  没有周期性 `/health` 探测 —— 一个「不报错但静默变慢」的节点只能靠真实流量把它打出来。
- `Transport` 无 `MaxConnsPerHost`、未设 `DisableCompression`；一致性哈希 key 仍可由客户端头
  `X-User-Id` 决定。
- `BreakerConfig` 的 `FailRatio` / `MinRequests` 零值仍不兜底：`FailRatio: 0` 意味着**一次失败即熔断**
  （批次 C 只修掉了「成功的请求也会把熔断器打开」，没有改零值语义本身）。
- 演示配置里 `self` 的零值 `BreakerConfig`（`FailRatio: 0`）与 `order-svc` 的权重 `3:1:1`
  （算法是 `round_robin`）都不生效。
- **无配置中心**：只有「文件 + 环境变量」，没有 etcd / Nacos 之类的下发与版本化；
  改 `SharedState.URL` / `ListenAddr` 仍需重启。
- **容器 / k8s 未在真实 daemon / 集群验证**（本机 Docker daemon 未启动）：`Dockerfile`、
  `deploy/docker-compose.yml`、`deploy/k8s/*.yaml` 已就位，但只做过静态核对。
- **压测报告已补**（D4）：`docs/性能压测报告.md` —— 基线 2077（`/healthz`）/ 1359（`/slow?ms=5`）RPS、
  并发拐点 c≈20、共享限流判定代价 ≈64%、闸门开销在噪声内；并记录了「压测工具不复用连接 → Windows
  临时端口耗尽 → 数字自相矛盾」的教训。CI 里的基准记录见 `.github/workflows/ci.yml`。
- **P2-3 / P2-4 / P2-8 仍未修**（编号见 `docs/网关缺陷审计与优化路线.md` §0 的「仍开放的 3 条 P2」）：
  P2-3 一致性哈希 key 仍可由客户端头 `X-User-Id` 决定（`config.go` 的 `HashKeyFrom`，客户端可自选落点，
  应改用**鉴权后的身份**）；P2-4 `Transport` 无 `MaxConnsPerHost`、未设 `DisableCompression`；
  P2-8 演示配置里 `self` 的零值 `BreakerConfig` 与 `order-svc` 的权重 `3:1:1` 不生效。
- **鉴权侧未加固（审计未编号，批次 C 期间核对代码新发现）**：API Key 仍支持 `?api_key=` query 传参
  （`internal/auth/auth.go` 的 `r.URL.Query().Get("api_key")`，密钥会进浏览器历史 / Referer / 上游访问日志）；
  JWT 无 `iss` / `aud` 校验、无 RS256/JWKS、无密钥轮换与吊销；`/metrics` 与 `/debug/logs` 自身无鉴权
  （靠「管理端口只对内网开放」兜住）。

> **已修（记录在此以免回退）**：`least_conn` 的平局偏置 —— 原实现按 map 迭代顺序取第一个最小值
> （`for addr, c := range b.conns`，Go 小 map 迭代起点随机但首元素占优），实测全 0 平局时
> 78% 集中到列表首节点（a:1=777 / b:1=116 / c:1=107，全 0 平局）；现按轮转起点取最小、平局轮流
> （999 次 = 333/333/333）。
>
> **批次 A：5 条 P0（按 `docs/网关缺陷审计与优化路线.md` 修复）**：
> 1. **协议升级（WebSocket）**：`statusWriter` 增加 `Unwrap() http.ResponseWriter`
>    （`internal/gateway/gateway.go:154`）与 `ReadFrom`（:158）—— `ReverseProxy` 用
>    `http.NewResponseController(rw).Hijack()` 取连接，Controller 只认 `Hijacker` 或 `Unwrap()`，
>    此前任何 `Connection: Upgrade` 请求都被答成 **502**；实测新增用例 `TestWebSocketUpgrade`
>    拿到 **101**，升级后双向数据可通。
> 2. **panic 兜底**：`ServeHTTP` 新增 ⑦′ panic 兜底 `defer`（`gateway.go:202-218`，
>    LIFO 下先于收口执行）；`/metrics` 新增 **`gw_panics_total`**（`internal/observability/observ.go:251`）。
> 3. **请求体上限 8MB**：`maxBodyBytes = 8 << 20`（`gateway.go:37`），路由匹配后、鉴权前按
>    `Content-Length` 拒绝 + `MaxBytesReader` 兜底；`proxy.ErrorHandler` 把超限错误映射成 **413**
>    （`internal/proxy/proxy.go:105-107`）；实测 raw socket 伪造 9MB `Content-Length` → `HTTP/1.1 413`。
> 4. **服务端读写超时**：`cmd/gateway/main.go:48` `ReadTimeout: 30s`、`:52`
>    `WriteTimeout: cfg.MaxServiceTimeout() + 15s`；新增 `config.MaxServiceTimeout()`
>    （`internal/config/config.go:19`）—— 实测生效，长流取舍见 §2。
> 5. **JWT 强制 `exp` + 寿命上限**：`internal/auth/auth.go` 新增 `ErrNoExpiry`（:32）与
>    `maxTokenLifetime = 24h`（:39）；`exp == 0` 直接拒（:143-144）；`iat` 存在时校验 `exp-iat ≤ 24h`；
>    `SignJWT` 自动补 `iat`，错误码新增 `token_no_expiry` / `token_too_long_lived`；实测无 `exp`
>    的 token 由原来的 **200** 变成 **401 `token_no_expiry`**。
>
> **批次 B：P1 9 条 + P2（P2-1 / P2-2 / P2-6 / P2-7）**：
> 1. **管理面独立监听（P1-6）**：新增 `Config.AdminListenAddr`（默认 `127.0.0.1:18081`）与
>    `Gateway.AdminHandler()`（`internal/gateway/gateway.go:201`）；`cmd/gateway/main.go:59` 起第二个
>    `http.Server`，优雅退出时一并 `Shutdown`（:86-88）。`/metrics`、`/debug/logs`、`/readyz` 不再在
>    业务端口 18080 提供（实测 **404**），18081 上三者均 **200**；`/readyz` 语义见 §2。
> 2. **节点级被动摘除（P1-2）**：新增 `internal/balancer/health.go`（四种算法共享的节点运行时状态）；
>    `Balancer` 接口加 `Report(addr, success)` 与 `Stats() []NodeStat`（`internal/balancer/balancer.go:113` 等）。
>    连续失败 ≥ `FailThreshold` → 摘除 `Cooldown`；成功一次立即回列；冷却到期只放行**一个**探测
>    （并发下也只有一个）；探测租约超时自愈；**全摘 fail-open**；`FailThreshold=0` 永不摘除。
>    主流水线除 `defer lb.Done` 外新增 `lb.Report(...)`（`gateway.go:495` / `:501` / `:512`）。
>    实测 19003 置恒 500 → 12 个 `/slow` = **500×3（全来自 19003）+ 200×9**，该节点 `failures=3 ejected=1`
>    → 再打 12 个落点 **19001:6 / 19002:6 / 19003:0** → 恢复 + 冷却后 `ejected=0`、落点回到 3/3/3。
> 3. **节点级指标（B3）**：`/metrics` 新增 `gw_upstream_inflight` / `gw_upstream_failures_total` /
>    `gw_upstream_ejected`（`gateway.go:272` 的 `nodeMetrics`，服务名排序输出）——
>    「least_conn 在途数不可观测」的老缺口关闭。
> 4. **启动期配置校验（P1-1）**：新增 `internal/config/validate.go`；`gateway.New` 第一件事就是
>    `cfg.Validate()`（`gateway.go:67`）—— 错误配置**启动即失败**，不再是运行期 500 `bad_config`。
>    覆盖重名路由、匹配条件完全相同、正则编译失败、上游服务不存在、`Transcode` 指向非 gRPC 节点、
>    服务 `Name` 与 key 不一致、`Timeout<=0`、节点重复/非 host:port、`Breaker.MinRequests>WindowSize`、
>    `FailRatio` 越界、`Ejection` 有阈值无冷却、`TrustedProxies` 非法 CIDR；空 `Host` 归一化成 `"*"`。
> 5. **可信代理真实 IP（P1-3）**：新增 `internal/clientip`（`Resolve` / `ParseTrusted`）；只有直连对端
>    本身是可信代理时才采信 XFF（从右往左跳过可信跳）或 `X-Real-IP`，否则完全不读。用于限流 key、
>    日志 `client=`、一致性哈希 key 回落（`gateway.go:327`）。新配置 `Config.TrustedProxies`（演示为 `nil`）。
> 6. **转码连接池（P1-4）**：per-address 拨号锁 + `grpc.NewClient`（去掉 `WithBlock`；
>    `internal/transcode/transcode.go:99-116`）→ 一个黑洞上游不再阻塞其它地址；
>    实测黑洞 `Conn` **544µs** 返回（旧实现会打满 300ms 预算）。
> 7. **路由头值正则预编译（P1-5）**：通配头值正则移到 `router.New`（`internal/router/route.go:63-91`）；
>    实测 **9698ns/55 allocs → 250ns/0 allocs（约 39×）**。
> 8. **访问日志（B8）**：`trace=` 改为**全量** 32 位，另加 `trace8=` 短前缀、`client=`、`upstream_ms=`
>    （与 `latency` 的差即网关自身开销；`internal/observability/observ.go:73-93`）。
> 9. **三处加固（P2-1 / P2-2 / P2-7）**：`InternalHeaders` 补 `Forwarded` / `X-Original-URL` /
>    `X-Rewrite-URL`（`internal/proxy/proxy.go:46-47`）；`StripPrefix` 加路径段边界
>    （`/apifoo` 不再被 `/api` 误剥，`proxy.go:70`）；JWT `Bearer` 前缀**大小写不敏感**
>    （RFC 7235，`internal/auth/auth.go:84`）。
> 10. **半开探测槽位（P2-6）**：熔断的 `Allow` / `Report` 改成 **defer 配对**（`gateway.go:448-453`），
>     任何提前 return 乃至 panic 都算「这次调用没成功」，顺带修掉半开探测槽位泄漏。
>
> **批次 C：C1~C6 六项能力 + 3 个缺陷**（配置外置/热重载、上游 TLS、每路由超时+重试、
> 共享状态服务、可观测、负载保护 —— 详见 `.workbuddy/memory/2026-09-30.md` 同名小节）：
> 1. **配置外置 + 热重载（C1）**：`internal/config/file.go` 的 `OverlayJSON` / `FromEnv` / `Load`
>    （`GW_CONFIG` → JSON 覆盖 → 环境变量 → 启动期校验）；`internal/gateway/reload.go` 的 `Reload` +
>    `cmd/gateway/main.go` 的 `watchConfig`（3s 轮询 mtime）。**实测**：把 `hot-svc` 由 19001 改到 19002，
>    重载后**仍是 503 `circuit_open`**（熔断器状态被保留；若被重建会返回 200），冷却后打到新上游
>    backend B；日志 `配置已热重载: routes=2 services=2 熔断器复用=2 均衡器重建=[hot-svc] 拓扑未变=[self] 服务删除=[] 代理清理=1`。
> 2. **上游 TLS（C2）**：`internal/config/tls.go` + `Upstream.Scheme` / `TLS`（`internal/config/types.go`），
>    HTTP 代理与 gRPC 转码**共用同一份 TLS 语义**。**实测**：配 CA → 200；不配 CA → **502**；
>    `insecure_skip_verify` → 200；坏 CA 路径 → **启动失败**。
> 3. **每路由超时 + 重试（C3）**：`internal/gateway/retry.go` 的 `planRetry`（三重闸门：路由配了 retry、
>    方法幂等或显式放行、**请求体为空**），`config.MaxServiceTimeout()` 把重试算进 `WriteTimeout`。
>    **实测**：`/retry` 5×200（日志 `err=retried(upstream refused)`）、`/noretry` 502、
>    `/slowroute?ms=1000`（路由超时 150ms）→ **504 / 163ms**。
> 4. **共享状态服务（C4）**：`internal/ratelimit/backend.go`、`internal/breaker/store.go`、
>    `internal/gateway/sharedstate.go`，参考实现 `cmd/statestore/main.go`。**实测**：两副本限流
>    `200,200,429,429`（本地对照 4×200）；杀掉状态服务后请求仍 200 且两个副本都是
>    `gw_shared_state_errors_total{backend="ratelimit"} 1`；熔断方面副本 B **自己只见过 1 次失败**
>    仍返回 503 `circuit_open`（1.2s 后）—— 采纳了副本 A 的跳闸。
> 5. **可观测（C5）**：`internal/observability/histogram.go`（秒级直方图 + 运行时指标 + 排序输出）、
>    `internal/observability/trace.go`（W3C traceparent）。**实测**：`gw_runtime_goroutines 9` /
>    `mem_alloc_bytes 623736` / `mem_sys_bytes 6836224` / `gc_cycles 0`；两次抓取（除 runtime 行）
>    **逐字节一致**；traceparent 同 trace-id、新 span-id（`ea436a45114500f5` → `35ed96d3ed632100`）、
>    `tracestate: vendor=abc`。
> 6. **负载保护（C6）**：新包 `internal/overload`（`Gate`：并发上限 + 有界排队），闸门在
>    `internal/gateway/gateway.go` 的「负载保护」段（**self 路由之后、鉴权之前**）。**实测**：
>    12 并发、`max_inflight=2` / `max_queue=0` → `{503:10, 200:2}`，`Retry-After=3`、
>    `X-Load-Shed=queue_full`、`gw_overload_rejected_total{route="slow"} 10`、`/healthz` 200。
>
> **批次 C 同时修掉三个缺陷**：
> ①**熔断器在零值 `FailRatio` 下成功的请求也会打开**（开闸条件缺 `failures > 0`：实测 `status=200`
> 却 `breaker=open`，几次后开始 503）—— `internal/breaker/breaker.go:202` 补上 `b.failures > 0`，
> 并为该包补 **6 个用例**（此前 `breaker` 包无测试）；
> ②**代理上游错误进不了访问日志 `err=`**（老缺口：`ErrorHandler` 改的是 `ReverseProxy` Clone 出去的
> Header）—— 改用 **context** 承载（`proxy.ErrHolder` / `WithErrHolder` / `ErrFrom`，
> `internal/proxy/proxy.go:196-241`），实测日志出现 `err=upstream refused` / `err=upstream timeout`；
> ③**P2-5：`Render` 三处未排序 map**（`internal/observability/observ.go:277` / `:322` 等）已排序输出，
> 并加回归用例「连渲染 5 次逐字节一致」。

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
