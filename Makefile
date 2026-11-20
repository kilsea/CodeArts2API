# 常用开发命令。Windows 上可在 Git Bash 中运行，或直接复制命令执行。
#
#   make check     # 提交前必跑：vet（含跨平台）+ test + 控制台检查（gofmt 由 CI 强制）
#   make build     # 带版本号的本地构建
#   make race      # 并发竞态检查（需要 cgo）
#   make docker    # 构建镜像

GO      ?= go
BIN     ?= codearts2api

# 版本号 = v + 构建日期 + 提交哈希（工作区有改动时加 -dirty）。
# 前缀 v 只是告诉人「这是个版本号」，不进排序逻辑；日期在前保证能按时间比较。
# 不手工维护语义版本：这个项目是「跟着上游漂移」的网关，手工改版本号只会
# 出现「版本说 0.2，行为是 0.3」的错位；日期+哈希能直接对应到某一次提交。
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DIRTY   ?= $(shell git status --porcelain 2>/dev/null | head -1)
VERSION ?= v$(shell date +%Y%m%d)-$(COMMIT)$(if $(DIRTY),-dirty)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILT)

.PHONY: all build test vet vet-linux fmt lint race console check clean docker docker-multiarch

all: check build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# 跨平台预检：在 Windows 上也能抓到「只在非 Windows 编译的文件」（如
# session_other.go）里的错误，以及「只在 Windows 定义、却被通用测试引用」的
# 符号 —— CI 的 linux job 就是挂在这类问题上。
vet-linux:
	GOOS=linux GOARCH=amd64 $(GO) vet ./...

fmt:
	$(GO) fmt ./...

# 可选：需要先安装 golangci-lint（CI 不强制依赖它）。
lint:
	golangci-lint run ./...

race:
	$(GO) test -race ./...

console:
	python tools/check_html_js.py

check: vet vet-linux test console

clean:
	rm -f $(BIN) $(BIN).exe

docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILT=$(BUILT) \
		-t $(BIN):$(VERSION) .

# 多架构镜像需要 buildx；日常交给 CI（.github/workflows/docker.yml）自动推 GHCR。
docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILT=$(BUILT) \
		--load -t $(BIN):$(VERSION) .
