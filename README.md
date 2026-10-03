# gateway（module gwlab）—— API 网关实验台

一台手写的七级 API 网关，用来把「请求进来之后到底经过了什么」变成可观测、
可复现的事实。除 `google.golang.org/grpc` 外只用 Go 标准库。

```
TraceID → ① 路由 → ② 鉴权 → ③ 限流 → ③ 熔断 → ④ 选节点 → ⑤ 转换 / ⑥ 转发 → ⑦ 指标 + 日志
```

两条硬约束：**鉴权必须在限流之前**（否则只能按 IP 限，NAT 后一栋楼共用一个桶）；
**熔断必须在选节点之前**（打开时要直接快速失败）。

## 目录结构

```
.
├── cmd/                        可执行入口
│   ├── gateway/                网关本体（业务端口 127.0.0.1:18080；管理端口 127.0.0.1:18081）
│   ├── backend/                演示上游：HTTP + gRPC + /__control 故障注入
│   ├── statestore/             参考共享状态服务（限流判定 + 熔断状态跨副本共享，审计 C4）
│   └── hashring/               一致性哈希测量程序（不参与请求链路）
├── internal/                   仅本模块可见的实现
│   ├── config/                 声明式配置模型 + 演示配置（9 条路由 / 6 个上游）+ 启动期校验（validate.go）
│   │                           + 外部 JSON 加载（file.go：覆盖语义 / 注释约定 / BOM 容忍）+ 上游 TLS（tls.go）
│   ├── router/                 ① 路由匹配：Host + Path(exact/prefix/regex) + Method + Header
│   ├── auth/                   ② 鉴权：JWT(HS256，手写) 与 API Key
│   ├── ratelimit/              ③ 限流：令牌桶 + 空闲桶回收 + 跨副本判定（backend.go）
│   ├── breaker/                ③ 熔断：closed/open/half-open 三态机 + 跨副本状态同步（store.go）
│   ├── overload/               ③′ 负载保护：并发上限 + 有界排队 + 过载 503（审计 C6）
│   ├── balancer/               ④ 负载均衡：轮询 / 加权 / 最少连接 / 一致性哈希 + 节点级被动摘除与指标（health.go）
│   ├── clientip/               可信代理链解析真实客户端 IP（XFF / X-Real-IP）
│   ├── transcode/              ⑤ 协议转换：REST/JSON → gRPC（per-address 拨号锁 + 非阻塞建连）
│   ├── proxy/                  ⑥ 反向代理：ReverseProxy 定制 + 请求头治理（上游错误用 context 回传）
│   ├── observability/          ⑦ 可观测性：TraceID / 访问日志 / Prometheus 指标
│   │                           + 直方图（histogram.go）+ W3C traceparent（trace.go）
│   └── gateway/                装配层：把上面七个模块接成一条流水线
│                               + 热重载（reload.go）/ 重试（retry.go）/ 共享状态（sharedstate.go）
├── scripts/                    压测与演练脚本（loadtest.py 压测 / demo_faults.py 故障演练 / demo_reload.py 热重载验收）
├── deploy/                     部署件：docker-compose.yml（含 statestore）、config.example.json 模板 +
│                               config.compose.json（演示栈实际加载的那份）、.env.example（密钥模板）、
│                               k8s/{gateway,backend,statestore}.yaml、observability/*
├── Dockerfile                  多阶段构建镜像（gateway/backend/statestore/hashring 进同一镜像）
├── .github/workflows/          CI（ci.yml：gofmt 卡口 + vet + go test -race + 三入口构建 + 基准记录）
├── docs/                       文档
│   ├── 项目地图.md               文件/依赖/链路/配置/改动定位的行号级索引（先看这份）
│   ├── 项目分析与执行链路.md      模块详解 + 真实实测输出
│   ├── 网关缺陷审计与优化路线.md   独立审计：P0/P1/P2 缺陷、对标标准网关、优化路线
│   └── 生产部署与接入新服务.md    生产部署、接入新服务、上线检查清单
├── go.mod / go.sum
├── Makefile
└── README.md
```

测试文件（`*_test.go`）与实现同目录；**12 个包有用例**：
`auth / balancer / breaker / clientip / config / gateway / observability / overload / proxy / ratelimit / router / transcode`。

## 快速开始

```bash
# 1) 三个演示上游（A 额外提供 gRPC）
go run ./cmd/backend -http 127.0.0.1:19001 -grpc 127.0.0.1:19100 -name A
go run ./cmd/backend -http 127.0.0.1:19002 -name B
go run ./cmd/backend -http 127.0.0.1:19003 -name C

# 2) 网关
go run ./cmd/gateway

# 或编译到 bin/
make build
```

### 用外部配置启动（配置外置 + 热重载）

`GW_CONFIG` 指向一份 JSON，覆盖内置默认值 —— 改上游不再需要重新编译（审计 C1）。
`deploy/config.example.json` 可直接加载，也展示了注释约定与 TLS 段：

```powershell
# Windows PowerShell（注意：Set-Content -Encoding UTF8 会写 BOM，加载器已容忍）
$env:GW_CONFIG = "D:\www\gateway\deploy\config.example.json"
go run ./cmd/gateway
```

- **覆盖语义**：`routes` / `services` 一写就**整体替换**内置默认值（只写要改的字段即可）；
  时长写成字符串（`"2s"` / `"500ms"`，整数纳秒也接受）；字段名拼错会**启动失败**
  （`DisallowUnknownFields` —— 静默忽略 `time_out` 这类拼写错误是配置外置最常见的线上事故）；
  **下划线开头的 key 递归当注释忽略**（JSON 没有注释语法，这是唯一的逃生口）；
  编辑器加的 UTF-8 BOM 会被剥掉。
- **环境变量优先级最高**（环境变量 > 文件 > 内置默认值）：`GW_LISTEN_ADDR`、`GW_ADMIN_LISTEN_ADDR`、
  `GW_JWT_SECRET`、`GW_API_KEY`、`GW_TRUSTED_PROXIES`（分隔符 `,` `;` 空格均可）。
  启动横幅会打印**配置来源**（`config  built-in（编译期默认值）` / 文件路径 / `+ 环境变量覆盖`）。
- **热重载**：进程每 3 秒轮询配置文件 mtime，变了就重新加载并**原地替换**配置快照
  （Windows 没有 SIGHUP，轮询两个平台行为一致）。重载**保留熔断器状态**（重建会把刚打开的熔断器
  重置回 closed）、拓扑未变时保留均衡器、按新配置清理代理缓存；JSON 写坏或校验不过时
  **保留旧配置**并打日志 —— 一次手抖不该把线上打挂。
  ⚠️ 改 `shared_state` 或 `listen_addr` / `admin_listen_addr` **需要重启**（热重载只替换配置快照，
  不重开监听，也不重建共享状态客户端）。

网关启动后会打印路由表，管理端点**默认在独立的管理端口**上提供（`AdminListenAddr`，默认
`127.0.0.1:18081`），启动横幅会多打一行 `admin http://127.0.0.1:18081  (/metrics  /debug/logs  /readyz)`：

| 端点 | 地址 | 说明 |
|---|---|---|
| `GET /metrics` | 管理端口 `18081` | Prometheus 文本指标：全局指标 + 节点级 `gw_upstream_inflight` / `gw_upstream_failures_total` / `gw_upstream_ejected`（服务名排序输出） |
| `GET /debug/logs` | 管理端口 `18081` | 最近 50 条结构化访问日志 |
| `GET /readyz` | 管理端口 `18081` | 就绪检查：任一**已被使用过**的服务处于熔断、或该服务所有节点被摘除 → **503**；否则 **200** 并列出每个服务的 `breaker/upstreams/ejected`（`/healthz` 只代表进程活着） |

> 管理端点已默认只在**独立的管理端口**提供（`AdminListenAddr`），业务端口 `18080` 不再暴露
> —— 实测 `18080 /metrics` 与 `18080 /readyz` 都是 **404**，`18081` 上 `/metrics`、`/debug/logs`、`/readyz`
> 都是 **200**；只有 `AdminListenAddr` 留空时才退回「与业务同端口」的老行为。生产仍应只让管理端口绑定回环/内网。

## 演示场景

配置见 `internal/config/config.go`，九条路由分别演示一个机制：

| 路由 | 场景 |
|---|---|
| `public-health` | 网关自答（`Upstream: self`） |
| `order-exact` / `order-prefix` | 精确匹配优先于前缀；前缀路由会剥掉 `/api` |
| `user-regex` | 正则匹配 |
| `user-mobile` | Header（`X-Client: mobile`）参与匹配 |
| `open-api` | API Key 鉴权 |
| `flaky` | 熔断三态演示（配合 `/__control?fail=1`） |
| `order-create` | HTTP → gRPC 协议转换 |
| `slow` | 最少连接（④）：配合后端 `?ms=` 人为延时占住连接才看得出差别 |

### 验证最少连接（`least_conn`）

`/slow` → `slow-svc`（`least_conn`，节点 `19001/19002/19003`）；演示后端的 `?ms=<毫秒>` 会给请求加人为延时（**上限 5000ms**），用它把连接真的占住。

**串行请求测不出 `least_conn`**：每次请求返回时在途计数都已归零，平局下节点会均匀轮转，和轮询看起来一样。必须并发打，或用长延时占住节点：

| 验证思路 | 做法 | 期望 |
|---|---|---|
| 并发分布均匀 | 9 个并发 `GET /slow?ms=1500` | A / B / C = **3 / 3 / 3**，墙钟 ≈1.52s（串行下限 13.5s） |
| 在途最少的节点优先 | 先用一个 `GET /slow?ms=4000` 占住某个节点，再串行发 20 个 `GET /slow?ms=1` | 被占住的节点**一次都不该被选中**（修复版实测 0/20） |

第二条是主流水线 `Pick`/`Done` 成对的回归口径：若漏掉 `Done()`，在途计数只增不减，对照版实测 6/20 会落到被占住的节点（A/B/C = 6/7/7，退化成≈轮询）。这类错误串行请求看不出来，`internal/balancer/balancer_test.go` 用单元测试兜住。

### 验证节点级摘除（被动离群摘除）

除了**服务级**熔断，网关还有**节点级**被动摘除（`internal/balancer/health.go`）：某个节点连续失败到
`Service.Ejection.FailThreshold` 就被摘掉 `Cooldown`；成功一次立即回列；冷却到期只放行**一个**探测
（并发下也只有一个），探测成功才真正回列。演示配置给 `order-svc` / `user-svc` / `slow-svc` 配了
`Ejection{FailThreshold: 3, Cooldown: 5s}`；`flaky-svc` 故意不配（它只有一个节点，那边演示的是服务级熔断）。

实测做法与数字：

```bash
curl -s "http://127.0.0.1:19003/__control?fail=1"                 # 让 19003 恒定 500
for i in $(seq 1 12); do curl -s -o /dev/null -w "%{http_code} " http://127.0.0.1:18080/slow; done
# → 500 × 3（三个都来自 19003）+ 200 × 9；连续失败 3 次后 19003 被摘除
curl -s http://127.0.0.1:18081/metrics | grep -E 'gw_upstream_(failures_total|ejected).*19003'
# → gw_upstream_failures_total{service="slow-svc",addr="127.0.0.1:19003"} 3
#   gw_upstream_ejected{service="slow-svc",addr="127.0.0.1:19003"} 1
for i in $(seq 1 12); do curl -s -o /dev/null -w "%{http_code} " http://127.0.0.1:18080/slow; done
# → 12 个全部 200，落点 19001:6 / 19002:6 / 19003:0（被摘的节点一次都不再被选中）
curl -s "http://127.0.0.1:19003/__control?fail=0"                 # 恢复
sleep 6                                                            # 等过 5s 冷却
# → gw_upstream_ejected{...19003} 0，落点回到 3/3/3
```

两条容易踩的点：**全摘时 fail-open**（所有节点都在冷却时 `Pick` 回退全量，不会把整个服务打死）；
`FailThreshold=0` 表示永不摘除（`flaky-svc` 走的就是这条）。

### 生成测试用 JWT

密钥为 `demo-secret-do-not-use-in-prod`（`internal/config/config.go`）：

```python
import hmac, hashlib, base64, json, time
def b64(b): return base64.urlsafe_b64encode(b).rstrip(b'=')
def sign(payload, secret="demo-secret-do-not-use-in-prod"):
    h = b64(json.dumps({"alg":"HS256","typ":"JWT"}, separators=(',',':')).encode())
    p = b64(json.dumps(payload, separators=(',',':')).encode())
    si = h + b'.' + p
    return (si + b'.' + b64(hmac.new(secret.encode(), si, hashlib.sha256).digest())).decode()

sign({"sub":"u-1","role":["admin","user"],"exp":int(time.time())+3600})  # 可访问 order-create
sign({"sub":"u-4","role":["viewer"],"exp":int(time.time())+3600})        # 403
```

### 验证熔断闭环

```bash
curl -s "http://127.0.0.1:19003/__control?fail=1"          # 注入故障
for i in $(seq 1 5); do curl -s -o /dev/null -w "%{http_code} " http://127.0.0.1:18080/flaky; done
sleep 3.3                                                   # 等冷却进入 half-open
curl -s "http://127.0.0.1:19003/__control?fail=0"          # 恢复
for i in $(seq 1 3); do curl -s -o /dev/null -w "%{http_code} " http://127.0.0.1:18080/flaky; done
```

> 限流验证不要用 `for curl` 循环：Windows 下每次 curl 进程启动约 200ms，
> 实际速率会低于阈值而打不出 429；用 Python 多线程才测得准。

### 验证热重载

用 `GW_CONFIG` 指向一份可写配置启动网关（脚本只改文件、不会起进程），然后跑验收脚本：

```powershell
$env:GW_CONFIG = "$env:TEMP\gw-hot.json"
python scripts/demo_reload.py     # 5 项断言，实测 5/5 通过
```

关键断言（**决定性的一条是"重载后仍熔断"**）：

1. 初始 `hot-svc → 19001`（backend A）200；
2. 把 19001 打到恒失败，熔断器打开 → 503 `circuit_open`；
3. 改写配置文件为 `19002`，等过 3 秒轮询窗口（实测等 4.5s）→ **仍然是 503 `circuit_open`**。
   若熔断器被重建，这里会变回 200 —— 熔断器状态必须跨重载保留，否则刚打开的熔断器会被重置回
   closed，流量立刻又打到还没恢复的上游；
4. 冷却（`OpenFor`）过后半开探测**打到新上游 backend B** 并恢复 200（说明配置确实换了、均衡器按新拓扑重建）；
5. 网关日志出现：
   `配置已热重载: routes=2 services=2 熔断器复用=2 均衡器重建=[hot-svc] 拓扑未变=[self] 服务删除=[] 代理清理=1`。

### 验证多副本共享限流 / 熔断

限流桶与熔断器默认都在**进程内** —— 多副本时限流放行量变成「副本数 × 阈值」，A 副本已熔断的服务
B 副本还在打流量（这正是"单副本压测通过、扩容出事"的来源）。配 `shared_state` 后两者跨副本共享：

```bash
# 1) 参考共享状态服务（协议：POST /ratelimit；POST+GET /breaker）
go run ./cmd/statestore -listen 127.0.0.1:18090
# 2) 两份配置分别监听 18080 / 18082，都指向 "shared_state": {"url": "http://127.0.0.1:18090"}
```

| 验证项 | 做法 | 期望（实测） |
|---|---|---|
| 限流跨副本 | 两个副本共同打同一条限流路由 | `200, 200, 429, 429`（**本地不共享时对照为 4×200**） |
| 熔断跨副本 | 副本 A 把服务打到跳闸，再打副本 B | 副本 B **自己只见过 1 次失败**，1.2s 后同样 503 `circuit_open`（采纳 A 的跳闸） |
| 降级不失败 | 杀掉 `statestore` 后继续打 | 请求仍 **200**；两个副本都出现 `gw_shared_state_errors_total{backend="ratelimit"} 1` |

降级语义是刻意的：读不到共享状态时限流按 `fail_open` 放行、熔断**退回本地状态机** ——
功能不丢，只是退回单副本语义，同时用 `gw_shared_state_errors_total{backend=...}` 把这件事变得可见。

### 验证负载保护

配置 `"overload": {"max_inflight": 2, "max_queue": 0}` 后，12 并发打慢路由：

| 期望 | 实测 |
|---|---|
| 并发上限内正常服务 | `200 × 2` |
| 超出立刻过载拒绝 | `503 × 10` |
| 过载响应头 | `Retry-After: 3`、`X-Load-Shed: queue_full` |
| 指标 | `gw_overload_rejected_total{route="slow"} 10`；另有 `gw_inflight_limit`、`gw_queue_waiting` |
| 健康检查不被拒 | `/healthz` → **200** |

闸门的位置是刻意的：在 **`/healthz` 等 self 路由之后、鉴权之前**。负载高时如果连健康检查也拒掉，
LB 会把实例摘走，剩下的实例压力更大，反而加速雪崩；而真正吃资源的鉴权 / 限流 / 转码 / 反代都在闸门之后。

**与限流的区别**（最容易混的一对概念）：限流管**速率**（每秒多少，维度是"谁在用"：用户 / IP / 路由）；
负载保护管**并发**（此刻多少在飞，维度是"网关与上游此刻扛不扛得住"）。上游慢 10 倍时请求速率一点没变，
限流完全不触发，只有并发保护能拦住这种雪崩。阈值怎么定：用 `scripts/loadtest.py` 找出不劣化前提下的
并发拐点，取其 70% 左右；`max_queue` 只用来吸收**突发**，别指望它扛持续过载。

## 生产部署

**生产只运行 `cmd/gateway` 一个进程。** `cmd/backend` 是演示替身（它的位置由你的真实
业务服务顶替，语言不限）、`cmd/hashring` 是离线测量工具，两者都不进生产环境。

**接入一个新的后端服务，只改 `internal/config/config.go` 一个文件**：

1. 在 `defaultServices()` 里加一个 `Service`（上游节点列表 + 负载均衡算法 + 熔断参数）
2. 在 `defaultRoutes()` 里加一条 `Route`（匹配规则 + 指向哪个服务 + 鉴权/限流）
3. `make build` 后重启 `bin/gateway`

路由与服务是解耦的：多条路由可指向同一个服务，加服务不涉及任何网关逻辑改动。

> 批次 C 之后多了一个选择：把路由与服务写进 `GW_CONFIG` 指向的 JSON（见「用外部配置启动」），
> 改完 3 秒内热重载生效，**不需要重新编译**。生产建议走这条路，`internal/config/config.go`
> 只留内置默认值。

> ⚠️ `BreakerConfig` 有零值陷阱：`FailRatio: 0` 意味着**一次失败即熔断**。不希望熔断的
> 服务必须显式配置 `FailRatio` 与 `MinRequests`。

上线前必须先处理：密钥改为环境变量注入（现在是源码里的演示值）、**确认没有 `prefix /` 的兜底路由**
（演示配置里的 `fallback` **已删除**，生产接入时不要加回——否则前端拼错路径会静默打到订单服务，404 永不触发）、
确认 `/metrics` 与 `/debug/logs` 不对外暴露。

批次 B 之后还要**额外确认三项**：

- **管理端口**：`AdminListenAddr`（默认 `127.0.0.1:18081`）必须只对内网/回环开放 —— `/metrics`、`/debug/logs`、
  `/readyz` 已默认不在业务端口（`18080`）提供，只有把 `AdminListenAddr` 留空才会退回同端口。
- **`TrustedProxies`**：必须与真实 LB / CDN 的网段一致。留空（`nil`）表示**不信任任何 XFF**（最安全）；
  写错了则限流维度会退化成 LB 地址（所有匿名客户端共用一个桶）。
- **`Ejection`**：`FailThreshold` / `Cooldown` 要按业务调 —— 阈值太小会因网络抖动就把节点摘掉，
  冷却太长又会让容量白白损失；`FailThreshold=0` 表示不启用节点级摘除。

批次 C 之后还要**额外确认三项**：

- **`shared_state`（多副本必配）**：多副本部署必须配 `shared_state.url`（参考实现
  `go run ./cmd/statestore -listen 127.0.0.1:18090`），否则限流放行量 = 副本数 × 阈值、
  A 副本已熔断的服务 B 副本还在打流量。同时注意：①状态服务本身是**新的单点**，
  必须能快速重启或水平扩展；②熔断是**周期性拉取**（`sync_interval`，默认 1s），
  副本之间生效有一个时间窗；③读不到时的降级行为要明确（限流按 `fail_open`、熔断退回本地状态机），
  并用 `gw_shared_state_errors_total{backend=...}` 告警。
- **`overload` 阈值**：`max_inflight` / `max_queue` 必须**按压测拐点定**，不要凭感觉填 ——
  用 `scripts/loadtest.py` 找出网关在不劣化前提下的并发拐点，取其 70% 左右；
  `max_queue` 只吸收突发（别指望它扛持续过载），`queue_timeout` / `retry_after` 要大于上游的典型耗时。
- **上游 TLS**：`Upstream.scheme` 为 `https` 时配 `tls{ca_file, server_name, insecure_skip_verify,
  client_cert_file, client_key_file}`；证书在**启动期**读入并解析，路径写错 / 证书私钥不匹配会
  **启动失败**（不会等到第一个请求才报握手错）；`insecure_skip_verify` 只应临时调试用。

同时确认网关自身的各项能力符合预期：**请求体上限 8MB**（超限返回 **413**）；`http.Server` 已设
**`ReadTimeout` 30s** 与 **`WriteTimeout` = 配置里最大 `Service.Timeout` + 15s**（⚠️ 超过该时长的长连接
流式响应（SSE）会被掐断，要长流需按路由放宽或用 `http.ResponseController.SetWriteDeadline` 续期）；
**JWT 必须带 `exp` 且寿命 ≤24h**（缺失 → 401 `token_no_expiry`，超长 → `token_too_long_lived`）；
`ServeHTTP` 里的 panic 会被兜底成 500 并计入 `/metrics` 的 **`gw_panics_total`**；
**配置写错会启动失败**（`gateway.New` 第一件事就是 `cfg.Validate()`，这是特性而不是故障）。

完整的改造清单、热重载实现细节与上线检查清单见
[`docs/生产部署与接入新服务.md`](docs/生产部署与接入新服务.md)。

## 已知问题

详见 `docs/项目分析与执行链路.md` 第 8 节，摘要：

- 代理链路的上游错误不会落进访问日志的 `err=` 字段（`Reverseproxy` 的 `ErrorHandler`
  收到的是深拷贝的出站请求，对其 Header 的修改传不回来）。
- ~~`least_conn` 已实现但无服务使用，且主流水线未调用 `balancer.Done()`；`fallback` 路由使 404 分支不可达。~~
  → **已修复**：`slow-svc`（`least_conn`）+ `/slow` 路由已接入，主流水线 `Pick`/`Done` 成对；均衡器平局偏置已修正（原按 map 迭代顺序取首个最小值，实测 78% 集中到首个节点，现按轮转起点取最小、999 次平局 = 333/333/333）；`fallback` 已删除，404 分支可达（`GET /nope/nothing` → 404 `route_not_found`）。单元测试见 `internal/balancer/balancer_test.go`。
- ~~`/metrics` 不暴露每个节点的在途计数，`least_conn` 的计数不变式没有指标可观测。~~
  → **已修复**：新增 `gw_upstream_inflight{service,addr}` / `gw_upstream_failures_total{service,addr}` /
  `gw_upstream_ejected{service,addr}`（`internal/gateway/gateway.go` 的 `nodeMetrics`，服务名排序输出），
  「least_conn 在途数不可观测」这个老缺口已关闭。
- **出站方向的 `X-Forwarded-For` 仍原样转发给上游**：本轮只修了**入站取值**——`internal/clientip`
  只在直连对端本身是可信代理时才采信 `X-Forwarded-For`（从右往左跳过可信跳）或 `X-Real-IP`，
  `TrustedProxies` 为 `nil` 时**一个字节都不读**；但网关出站时仍会把客户端给的 XFF 原样带给上游。
- **无主动健康探活**：只有**被动**摘除（`Ejection`）与服务级熔断，没有周期性 `/health` 探测 ——
  一个「不报错但静默变慢」的节点只能靠真实流量把它打出来。
- `Transport` 无 `MaxConnsPerHost`，也未设 `DisableCompression`；一致性哈希 key 仍可由客户端头
  `X-User-Id` 决定。
- 演示配置里 `self` 的零值 `BreakerConfig`（`FailRatio: 0` 即一次失败就熔断的语义）与
  `order-svc` 的权重 `3:1:1`（算法是 `round_robin`）都不生效。
- **无配置中心**：只有「文件 + 环境变量」，没有 etcd / Nacos 之类的下发与版本化；
  且改 `shared_state` 或 `listen_addr` / `admin_listen_addr` 仍需重启（热重载只换配置快照）。
- **k8s 清单未在真实集群验证**（compose 演示栈已在 Docker Desktop 实测跑通，两处部署即坏的坑已修：
  镜像缺 `statestore` 二进制、compose 用 `command` 覆盖入口被 `ENTRYPOINT` 追加）：
  `deploy/k8s/*.yaml` 只做过离线结构核对，本机没启用 Docker Desktop 的 Kubernetes。详见 `docs/生产部署与接入新服务.md` §4。
- **限流阈值改了不刷新已存在的令牌桶**：`Bucket(key, p)` 命中旧桶就原样返回，新 `rate/burst` 要等空闲 3 分钟
  GC 或进程（含 `statestore`）重启才生效 —— 容器里热重载后「限流数字看着没变」就是它。
- **P2 剩余项**：API Key 仍支持 `?api_key=` query 传参（会进浏览器历史 / Referer / 上游日志）；
  无 `iss` / `aud` 校验、无 RS256/JWKS、无密钥轮换与吊销；`/metrics` 与 `/debug/logs` 自身无鉴权
  （靠「管理端口只对内网开放」兜住）。

其中「多副本状态外化」已由 `shared_state` + `cmd/statestore` 关闭，其余改造方案与完整检查清单见
[`docs/生产部署与接入新服务.md`](docs/生产部署与接入新服务.md)。

> 本列表只记项目自身在 `docs/项目分析与执行链路.md` 第 8 节列出的缺口。完整的独立审计
> （P0/P1/P2 与对标标准网关的能力矩阵）见 [`docs/网关缺陷审计与优化路线.md`](.workbuddy/tmp/网关缺陷审计与优化路线.md)；
> 其中 5 条 P0 已于批次 A 修复，**批次 B 的 9 条 P1 + 3 条 P2 也已修复**（管理面独立监听、启动期配置校验、
> 节点级被动摘除、可信代理真实 IP、非阻塞拨号、头值正则预编译、节点级指标、访问日志全量 trace、三处加固）；
> **批次 C（C1~C6）也已全部实现并验证**（配置外置 + 热重载、上游 TLS、每路由超时 + 重试、共享状态服务、
> 直方图 + W3C traceparent + 运行时指标、负载保护），另修掉熔断零值误跳闸、代理上游错误不进日志与 P2-5
> （`Render` 三处 map 未排序）三个问题 —— 详见下方「已修复」摘要与
> [`.workbuddy/memory/2026-09-30.md`](.workbuddy/memory/2026-09-30.md)。

> **已修复摘要（批次 C，记录在此以免回退）**：
> ①**配置外置 + 热重载**：`GW_CONFIG` 加载 JSON（覆盖语义 / `DisallowUnknownFields` / 下划线 key 当注释 /
> 容忍 BOM），环境变量优先，启动打印配置来源；轮询 mtime（3s）热重载，**保留熔断器状态**、只在拓扑变化时
> 重建均衡器、按新配置清理代理缓存，失败保留旧配置。②**上游 TLS**：`Upstream.scheme` + `tls{...}`，
> HTTP 代理与 gRPC 转码共用同一份 TLS 语义，证书启动期校验，**绝不静默降级**。
> ③**每路由超时 + 重试**：只重试「未写出任何字节」的失败、默认只幂等方法、请求体必须为空、5xx 不重试；
> `WriteTimeout` 已把重试算进去。④**共享状态服务**：限流判定与熔断打开状态跨副本共享，
> 读不到时降级并计入 `gw_shared_state_errors_total{backend=...}`。⑤**可观测**：秒级直方图
> `gw_request_duration_seconds_*`（可跨实例聚合算分位）+ 运行时指标 + `/metrics` 稳定排序 + W3C
> `traceparent`（沿用 trace-id、为本跳生成新 span-id、`tracestate` 透传）+ 日志 `span=`。
> ⑥**负载保护**：并发上限 + 有界排队 + 过载 503（`Retry-After` / `X-Load-Shed`），闸门在 self 路由之后、
> 鉴权之前。另修：熔断器零值 `FailRatio` 下成功请求也会跳闸（开闸条件缺 `failures > 0`）、
> 代理上游错误进不了日志 `err=`（改用 `proxy.ErrHolder` 走 context）、`Render` 三处未排序 map。

## 开发

```bash
make fmt    # gofmt
make vet    # go vet
make test   # go test（12 个包：auth / balancer / breaker / clientip / config / gateway / observability
            #          / overload / proxy / ratelimit / router / transcode）
make build  # 编译到 bin/

# 参考共享状态服务（验证多副本共享限流/熔断时要它，默认监听 127.0.0.1:18090）
go run ./cmd/statestore -listen 127.0.0.1:18090
```

规模：**52 个 `.go` / 10062 行**（测试 24 个 / 4397 行，生产 28 个 / 5665 行）。
除单测外还有 fuzz（`FuzzMatch` 88.3 万次、`FuzzVerifyJWT` 44.7 万次无 panic（各跑 60 秒））与端到端演练
（`scripts/demo_faults.py` 16/16、`scripts/demo_reload.py` 5/5）。

压测：`python scripts/loadtest.py --path /healthz -c 20 -d 3`（纯标准库，不需要 wrk/k6；
实测 `/healthz` **1804 RPS**（p50 8.5 / p95 14.9 / p99 25.8 ms），全链路 `/slow?ms=5` **934 RPS**
（p50 16.1 / p99 39.9 ms），脚本同时给出客户端 CPU 占比以免把客户端瓶颈误读成网关瓶颈）。

本机 `go test -race` **不可用**（`CGO_ENABLED=0` 且无 gcc，报 `-race requires cgo`），
竞态检测在 CI（`.github/workflows/ci.yml`，Linux）上跑。
