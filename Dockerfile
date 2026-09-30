# 多阶段构建：builder 里编三个入口，runtime 只带二进制。
#
# 监听地址/密钥等由**配置外置**（批次 C1）注入，容器里这样用：
#   -e GW_LISTEN_ADDR=0.0.0.0:18080 -e GW_ADMIN_LISTEN_ADDR=0.0.0.0:18081
#   -e GW_CONFIG=/etc/gateway/config.json（可选，挂一份 JSON 进去；语义是覆盖而非合并）
# 不注入时退回内置默认值 127.0.0.1:18080 / :18081（**容器里绑回环会导致容器外访问不到**，
# 所以容器/编排里务必显式给 GW_LISTEN_ADDR）。backend 不受影响（它用 -http/-grpc flag）。
FROM golang:1.26-alpine AS builder

WORKDIR /src

# 先只拷依赖清单，让 go mod download 这层能被缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0：静态二进制，落到 scratch/distroless 里也能跑
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/backend ./cmd/backend \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/hashring ./cmd/hashring

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/ /app/

# 业务端口 18080 + 管理端口 18081（批次 B4 起管理端点独立监听）
EXPOSE 18080 18081

USER nonroot:nonroot
WORKDIR /app

# 生产只跑 cmd/gateway（见 AGENTS.md §4）；backend / hashring 由 compose 覆盖 command 使用
ENTRYPOINT ["/app/gateway"]
