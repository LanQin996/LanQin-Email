# 开发环境设置指南

## 前置要求

- Go 1.25+
- Node.js 20+
- pnpm 10.28.2 (通过 corepack 启用)
- Docker 和 Docker Compose (用于部署测试)

## 快速开始

### 1. 克隆项目

```bash
git clone https://github.com/LanQin996/LanQin-Email.git
cd LanQin-Email
```

### 2. 设置后端（API）

```bash
cd apps/api
go mod download
go test ./...
go run ./cmd/server
```

后端将在 `http://localhost:8080` 运行

### 3. 设置前端（Web）

在新终端中：

```bash
cd apps/web
corepack enable
corepack prepare pnpm@10.28.2 --activate
pnpm install
pnpm run dev
```

前端将在 `http://localhost:5173` 运行

默认管理员邮箱是 `admin@lanqin.local`。在开发环境中，明确设置 `LANQIN_ADMIN_PASSWORD`；如果未设置，后端会在首次启动时生成随机密码并打印到日志中。

## 代码质量工具

### Go 工具

```bash
# 运行测试
make api-test

# 静态分析
make api-lint

# 格式化代码
make api-fmt

# 运行所有检查
make api-check
```

### 前端工具

```bash
# 安装依赖
make web-install

# Lint 检查
make web-lint

# 格式化代码
make web-fmt

# 运行所有检查
make web-check
```

### 一键命令

```bash
# 运行所有测试
make test

# 运行所有 lint 检查
make lint

# 格式化所有代码
make fmt

# 运行所有检查
make check
```

## VSCode 推荐设置

在 `.vscode/settings.json` 中添加：

```json
{
  "editor.formatOnSave": true,
  "editor.codeActionsOnSave": {
    "source.fixAll.eslint": true
  },
  "[typescript]": {
    "editor.defaultFormatter": "esbenp.prettier-vscode"
  },
  "[typescriptreact]": {
    "editor.defaultFormatter": "esbenp.prettier-vscode"
  },
  "[go]": {
    "editor.defaultFormatter": "golang.go"
  },
  "go.lintTool": "staticcheck",
  "go.lintOnSave": "workspace"
}
```

## VSCode 推荐扩展

- Go (golang.go)
- ESLint (dbaeumer.vscode-eslint)
- Prettier (esbenp.prettier-vscode)
- Tailwind CSS IntelliSense (bradlc.vscode-tailwindcss)

## Git 提交前检查

在提交代码前，确保运行：

```bash
make check
```

这会自动运行：
- shadcn/ui 使用检查
- ESLint 检查
- Prettier 格式检查
- TypeScript 类型检查
- Vite 构建
- Go 测试

## 常见问题

### Q: 前端代理 API 请求失败？

确保后端在 `http://localhost:8080` 运行。Vite 配置中已设置代理。

### Q: Go 测试在 Windows 上有文件锁定问题？

这是已知问题。测试本身通过了，只是临时文件清理失败。我们正在修复中。

### Q: 如何运行单个测试？

```bash
# Go
cd apps/api
go test -run TestName ./...

# Web（如有测试框架）
cd apps/web
pnpm test -- TestName
```

## 贡献指南

1. Fork 项目
2. 创建特性分支 (`git checkout -b feature/amazing-feature`)
3. 提交更改 (`git commit -m 'Add amazing feature'`)
4. 推送到分支 (`git push origin feature/amazing-feature`)
5. 打开 Pull Request

所有 PR 必须通过 CI 检查才能合并。
