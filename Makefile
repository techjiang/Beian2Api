SHELL := /bin/bash
APP   := beian2api
VER   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VER)

.PHONY: all build run test race lint fmt vet clean release icons docker

all: build

build: ## 本地编译
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) .

run: ## 本地运行
	go run . serve

test: ## 单元测试
	go test ./...

race: ## 竞态检测
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: fmt vet test

icons: ## 重新生成 PNG 图标
	python3 scripts/gen-icons.py \
		internal/api/assets/apple-touch-icon.png:180 \
		internal/api/assets/favicon.png:32 \
		docs/icon-512.png:512

release: ## 交叉编译全部平台到 dist/
	@bash scripts/build.sh

docker: ## 构建容器镜像
	docker build -t beian2api:$(VER) .

clean:
	rm -rf bin dist
