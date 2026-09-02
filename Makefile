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

# Docker 开发环境命令
.PHONY: dev-up dev-down dev-logs dev-restart dev-clean docker-build

dev-up: ## 启动开发环境
	docker compose -f docker-compose.dev.yml up -d
	@echo "开发环境已启动"
	@echo "Web: http://localhost:5173"
	@echo "API: http://localhost:8080"

dev-up-postgres: ## 启动开发环境（使用 PostgreSQL）
	docker compose -f docker-compose.dev.yml --profile postgres up -d

dev-up-mysql: ## 启动开发环境（使用 MySQL）
	docker compose -f docker-compose.dev.yml --profile mysql up -d

dev-up-all: ## 启动开发环境（包含所有服务）
	docker compose -f docker-compose.dev.yml --profile postgres --profile mysql --profile mail-testing up -d

dev-down: ## 停止开发环境
	docker compose -f docker-compose.dev.yml down

dev-logs: ## 查看开发环境日志
	docker compose -f docker-compose.dev.yml logs -f

dev-restart: ## 重启开发环境
	docker compose -f docker-compose.dev.yml restart

dev-clean: ## 停止并清理开发环境（删除数据卷）
	docker compose -f docker-compose.dev.yml down -v
	@echo "开发环境已清理"

docker-build: ## 构建 Docker 镜像
	docker build -f deploy/all-in-one/Dockerfile -t lanqin-email:latest .

docker-build-optimized: ## 构建优化的 Docker 镜像（Alpine 基础）
	docker build -f Dockerfile.optimized -t lanqin-email:optimized .

docker-build-dev: ## 构建开发 Docker 镜像
	docker build -f Dockerfile.dev -t lanqin-email:dev .
