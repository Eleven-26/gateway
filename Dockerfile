# 多阶段构建：builder 里编入口，runtime 只带二进制。
#
# 监听地址/密钥等由配置外置注入，容器里这样用：
#   -e GW_LISTEN_ADDR=0.0.0.0:18080 -e GW_ADMIN_LISTEN_ADDR=0.0.0.0:18081
#   -e GW_CONFIG=/etc/gateway/config.json（可选，挂一份 JSON 进去；语义是覆盖而非合并）
# 不注入时退回内置默认值 127.0.0.1:18080 / :18081（**容器里绑回环会导致容器外访问不到**，
# 所以容器/编排里务必显式给 GW_LISTEN_ADDR）。backend / statestore 不受影响（它们用 flag）。
#
# 运行期基镜像可换（必须在第一个 FROM 之前声明，BuildKit 才认 FROM ${...}）：
#   docker build --build-arg RUNTIME_IMAGE=gcr.nju.edu.cn/distroless/static-debian12:nonroot -t gateway:dev .
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM golang:1.26-alpine AS builder

WORKDIR /src

# 先只拷依赖清单，让 go mod download 这层能被缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0：静态二进制，落到 scratch/distroless 里也能跑
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/backend ./cmd/backend \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/statestore ./cmd/statestore \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/hashring ./cmd/hashring

# 运行期：distroless/static，只带二进制与 CA 证书（无 shell —— compose 的 healthcheck 因此探不了 HTTP）
FROM ${RUNTIME_IMAGE}

COPY --from=builder /out/ /app/

# 业务端口 18080 + 管理端口 18081（管理端点独立监听）
EXPOSE 18080 18081

USER nonroot:nonroot
WORKDIR /app

# 生产只跑 cmd/gateway（见 AGENTS.md §4）；backend / statestore / hashring 由 compose 覆盖 command 使用
ENTRYPOINT ["/app/gateway"]
