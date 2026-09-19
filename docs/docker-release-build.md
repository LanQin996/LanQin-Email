# Docker Release 构建与回滚

发布保留 `linux/amd64` 和 `linux/arm64`，不改变部署镜像名称和版本标签。

- API 构建阶段使用 `$BUILDPLATFORM` 的 Go，通过 `TARGETOS` / `TARGETARCH`
  交叉编译；`CGO_ENABLED=0` 保持不变。运行镜像仍使用目标架构。
- Web 构建阶段使用宿主架构的 Node，静态资源可以供两种架构共用。
- QEMU 仅用于非宿主架构运行镜像中的包安装等命令，不用于 Go / Node 编译。
- 每个 GHCR 镜像使用自己的 `:buildcache` 标签保存中间层，避免不同镜像竞争
  同一个缓存，并允许不同发布 tag 复用缓存。`buildcache` 不是部署标签。
- Go 模块保留在依赖层中；registry 缓存不会保存 `RUN --mount=type=cache`
  的内容。源码变化时可以复用依赖层，但全新的 runner 仍可能需要重新编译 Go 包。

首次运行需要填充缓存，不保证达到热缓存耗时。`mode=max` 会保存构建源码等
中间层，因此该方案面向本公开仓库；不可向构建上下文添加凭据或私有邮件数据。
不要把 `:buildcache` 用作运行镜像，也不要在仍需加速发布时清理该标签。

## 发布验证

在有 Docker Engine 的环境中，先运行不推送镜像的验证：

```bash
docker buildx build --platform linux/amd64,linux/arm64 --file deploy/api.Dockerfile --output type=cacheonly .
docker buildx build --platform linux/amd64,linux/arm64 --file deploy/web.Dockerfile --output type=cacheonly .
docker buildx build --platform linux/amd64,linux/arm64 --file deploy/all-in-one/Dockerfile --output type=cacheonly .
```

非原生平台需要已配置 QEMU 的 builder；发布工作流会自动配置。
发布后使用 `docker buildx imagetools inspect <镜像>:<版本>` 核对两种架构。
分别在 AMD64、ARM64 环境启动合成测试配置，检查 API readiness 和前端加载。
比较 Actions 的构建日志、缓存命中和 `Build and push` 耗时，不能只依据总时长
判断优化效果。

## 回滚

构建问题可同时回退 `.github/workflows/docker.yml`、`deploy/api.Dockerfile`、
`deploy/web.Dockerfile`、`deploy/all-in-one/Dockerfile` 的本次优化，再使用新的
版本 tag 发布；不要移动已发布 tag。旧发布流程不使用 `:buildcache`，无需删除
该缓存即可回退。运行环境需要回滚时，将镜像固定到上一已验证版本，保留现有
配置和数据卷；本次修改不包含数据库迁移。
