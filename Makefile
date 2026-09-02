.PHONY: help test lint fmt check build clean

help: ## 显示此帮助信息
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# Go API 相关命令
.PHONY: api-test api-lint api-fmt api-check

api-test: ## 运行 API 测试
	cd apps/api && go test -v ./...

api-lint: ## 运行 Go 静态分析
	cd apps/api && staticcheck ./...

api-fmt: ## 格式化 Go 代码
	cd apps/api && go fmt ./...

api-check: api-fmt api-lint api-test ## 运行所有 API 检查

# Web 前端相关命令
.PHONY: web-install web-lint web-fmt web-check web-build

web-install: ## 安装前端依赖
	cd apps/web && pnpm install

web-lint: ## 运行前端 lint
	cd apps/web && pnpm run lint

web-fmt: ## 格式化前端代码
	cd apps/web && pnpm run format

web-check: ## 运行所有前端检查
	cd apps/web && pnpm run check

web-build: ## 构建前端
	cd apps/web && pnpm run build

# 全项目命令
.PHONY: test lint fmt check build

test: api-test ## 运行所有测试
	@echo "所有测试完成"

lint: api-lint web-lint ## 运行所有 lint 检查

fmt: api-fmt web-fmt ## 格式化所有代码

check: api-check web-check ## 运行所有检查

build: web-build ## 构建所有组件
	@echo "构建完成"

clean: ## 清理构建文件
	rm -rf apps/web/dist
	rm -rf apps/api/tmp
	@echo "清理完成"
