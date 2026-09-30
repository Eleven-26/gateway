# gw-go（module gwlab）构建与检查
#
# Windows 下用 Git Bash 或 make（如未安装，可直接用 README 中的 go 命令）。

BIN_DIR := bin

.PHONY: all build gateway backend hashring fmt vet test tidy run-gateway run-backend clean

all: build

build: gateway backend hashring

gateway:
	go build -o $(BIN_DIR)/gateway ./cmd/gateway

backend:
	go build -o $(BIN_DIR)/backend ./cmd/backend

hashring:
	go build -o $(BIN_DIR)/hashring ./cmd/hashring

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy

run-gateway:
	go run ./cmd/gateway

run-backend:
	go run ./cmd/backend -http 127.0.0.1:19001 -grpc 127.0.0.1:19100 -name A

clean:
	rm -rf $(BIN_DIR)
