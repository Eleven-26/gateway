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
│   ├── gateway/                网关本体（监听 127.0.0.1:18080）
│   ├── backend/                演示上游：HTTP + gRPC + /__control 故障注入
│   └── hashring/               一致性哈希测量程序（不参与请求链路）
├── internal/                   仅本模块可见的实现
│   ├── config/                 声明式配置模型 + 演示配置（9 条路由 / 5 个上游）
│   ├── router/                 ① 路由匹配：Host + Path(exact/prefix/regex) + Method + Header
│   ├── auth/                   ② 鉴权：JWT(HS256，手写) 与 API Key
│   ├── ratelimit/              ③ 限流：令牌桶 + 空闲桶回收
│   ├── breaker/                ③ 熔断：closed/open/half-open 三态机
│   ├── balancer/               ④ 负载均衡：轮询 / 加权 / 最少连接 / 一致性哈希
│   ├── transcode/              ⑤ 协议转换：REST/JSON → gRPC（含连接池）
│   ├── proxy/                  ⑥ 反向代理：ReverseProxy 定制 + 请求头治理
│   ├── observability/          ⑦ 可观测性：TraceID / 访问日志 / Prometheus 指标
│   └── gateway/                装配层：把上面七个模块接成一条流水线
├── docs/                       文档
│   ├── 项目地图.md               文件/依赖/链路/配置/改动定位的行号级索引（先看这份）
│   ├── 项目分析与执行链路.md      模块详解 + 真实实测输出
│   └── 生产部署与接入新服务.md    生产部署、接入新服务、上线检查清单
├── go.mod / go.sum
├── Makefile
└── README.md
```

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

网关启动后会打印路由表，并暴露两个自带端点：

| 端点 | 说明 |
|---|---|
| `GET /metrics` | Prometheus 文本指标（不经过路由与鉴权） |
| `GET /debug/logs` | 最近 50 条结构化访问日志 |

> ⚠️ 这两个端点在生产中必须加访问控制或只监听内网。

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
| `fallback` | 兜底（因此 404 分支在当前配置下不可达） |

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

## 生产部署

**生产只运行 `cmd/gateway` 一个进程。** `cmd/backend` 是演示替身（它的位置由你的真实
业务服务顶替，语言不限）、`cmd/hashring` 是离线测量工具，两者都不进生产环境。

**接入一个新的后端服务，只改 `internal/config/config.go` 一个文件**：

1. 在 `defaultServices()` 里加一个 `Service`（上游节点列表 + 负载均衡算法 + 熔断参数）
2. 在 `defaultRoutes()` 里加一条 `Route`（匹配规则 + 指向哪个服务 + 鉴权/限流）
3. `make build` 后重启 `bin/gateway`

路由与服务是解耦的：多条路由可指向同一个服务，加服务不涉及任何网关逻辑改动。

> ⚠️ `BreakerConfig` 有零值陷阱：`FailRatio: 0` 意味着**一次失败即熔断**。不希望熔断的
> 服务必须显式配置 `FailRatio` 与 `MinRequests`。

上线前必须先处理：密钥改为环境变量注入（现在是源码里的演示值）、**删掉 `fallback` 路由**
（否则前端拼错路径会静默打到订单服务，404 永不触发）、给 `/metrics` 与 `/debug/logs` 加访问控制。

完整的改造清单、热重载实现细节与上线检查清单见
[`docs/生产部署与接入新服务.md`](docs/生产部署与接入新服务.md)。

## 已知问题

详见 `docs/项目分析与执行链路.md` 第 8 节，摘要：

- 代理链路的上游错误不会落进访问日志的 `err=` 字段（`Reverseproxy` 的 `ErrorHandler`
  收到的是深拷贝的出站请求，对其 Header 的修改传不回来）。
- `least_conn` 已实现但无服务使用，且主流水线未调用 `balancer.Done()`。
- `order-svc` 配了权重 `3:1:1`，但算法是 `round_robin`，权重不生效。
- `fallback` 路由使 404 分支不可达。
- 入站 `X-Forwarded-For` 不清理，可伪造；取真实 IP 必须用 `X-Real-IP`。
- `BreakerConfig` 的 `FailRatio` / `MinRequests` 零值不兜底，`FailRatio: 0` 意味着一次失败即熔断。
- 限流桶、熔断器、指标全在单进程内存，多副本部署需外部化。

其中 `fallback` 路由与多副本状态外化属于「上线前必须解决」，改造方案与完整检查清单见
[`docs/生产部署与接入新服务.md`](docs/生产部署与接入新服务.md)。

## 开发

```bash
make fmt    # gofmt
make vet    # go vet
make test   # go test（当前无测试用例）
make build  # 编译到 bin/
```
