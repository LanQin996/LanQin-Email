# 多地点上传（Issue #34，M2 首批）

在 [快照 CLI](backup.md) 的基础上，`upload` 将**已经完成的同一份密文**上传到多个 S3 / WebDAV / Google Drive 实例。它不会访问应用数据库、生成在线快照、停止服务、使用恢复私钥或删除远端内容。[管理端](backup-admin.md) 已接通新建快照、上传任务、每日计划和 Drive OAuth。`fetch` 支持显式下载并校验密文；Web 云端列表/恢复向导和自动保留策略尚未实现。

## 配置

参考 [多地点配置样例](backup-destinations.example.json)。样例包含 3 个 S3、2 个 WebDAV，各自拥有独立 ID、名称、连接配置和环境变量引用；`.invalid` 地址必须替换为实际地址。相同类型、endpoint、bucket 都可以重复，只有 `id` 必须唯一。当前单次配置上限为 64 个地点，不是 S3 只能添加一个。

- CLI 配置只保存 `access_key_id_env`、`secret_access_key_env`、可选的 `session_token_env` 或 `username_env` / `password_env`，这些字段的值是环境变量名称，**不是凭据本身**。通过受保护的运行环境提供对应变量，不能把真实凭据写入此样例、命令行或仓库。管理端加密凭据库与此 CLI 配置目前独立，尚未互相导出或自动对接；本 CLI 不读取应用凭据库，也不替操作者加密外部环境配置。
- S3 使用官方 AWS SDK for Go v2，逐地点显式提供凭据；不使用默认 AWS 凭据链、共享配置文件或实例元数据凭据。分别设置 HTTPS `endpoint`、`region`、`bucket`、可选 `prefix` 和 `path_style`。只支持普通桶，不支持 ARN / Access Point / S3 Express 特殊端点。
- `prefix` 不带开头/结尾的 `/`，只允许 ASCII 字母、数字、`_`、`-`、`.` 及目录分隔符，禁止 `.` / `..` 段。桶必须预先存在。
- WebDAV 的 `url` 是**已存在的集合目录**，支持 Basic Authentication，通过 HTTPS 传输。不会自动创建目录、协商其他认证方式或跟随重定向。需要应用密码的供应商应由操作者提供应用密码。
- `disabled: true` 表示上传时跳过该地点；与管理端独立的每日计划开关不同。显式 `fetch` 可读取停用地点以便恢复，但不启用它或修改计划。
- 配置文件必须是单个 version 1 JSON 值，最多 1 MiB，拒绝未知字段。完整配置结构及选择的 ID 会在任何上传前校验；单个地点缺少运行时凭据仅使该地点失败，不影响其他有效地点。

## 手动执行

先按照离线快照文档构建 CLI 并运行 `create`，独立可信地保存它输出的 SHA-256 收据。然后在已配置凭据环境、且暂存目录权限合适的进程中执行：

```text
./tmp/lanqin-backup upload --config /secure/config/destinations.json --in /backups/sample.age --expected-sha256 <trusted-64-hex-receipt> --spool-dir /secure/spool
```

默认对所有未停用的地点上传。只选其中几个地点：

```text
./tmp/lanqin-backup upload --config /secure/config/destinations.json --in /backups/sample.age --expected-sha256 <trusted-64-hex-receipt> --destinations r2,b2 --concurrency 2 --attempts 3 --timeout 20m --spool-dir /secure/spool
```

`--spool-dir` 必须存在；不指定时使用系统临时目录。上传前将输入复制到一个私有临时密文文件，对**实际复制的数据**校验收据；各地点及重试通过独立读取器读取同一副本，不会共享文件偏移或因原文件变化而上传不同内容。暂存区需有至少一份密文的可用空间，完成或失败后只移除本次创建的暂存文件，不删除源备份。Linux 文件权限为 `0600`；Windows 仍需操作者配置合适的目录 ACL。

仅接受 regular file 和二进制 age 格式前缀，拒绝符号链接、明显的明文或不匹配的收据。前缀检查不是 age 认证，也不证明文件内含有效的 LanQin 快照；完整格式及加密认证仍在 `restore` 时检查。

限制：

- 每次 CLI 调用默认并发 2 个地点，可设置 1..8；多个独立进程之间目前没有全局调度或限流。
- 单地点最多 3 次尝试，只对网络错误、超时、限流和部分服务端错误进行有界退避重试。认证失败、重定向、已存在对象等不自动重试。取消操作会中止运行请求并跳过待执行任务。
- 每次尝试默认超时 20 分钟，最多 1 小时。默认密文上限 1 GiB，`--max-bytes` 最多 5 GiB；当前是单 PUT，不支持 multipart / 断点续传，不能用于超过该限制的归档。
- 每次尝试生成独立随机对象名 `lanqin-<128-bit-random>.age`，并发送 `If-None-Match: *`，不降级成覆盖式上传。S3 额外使用 `Content-MD5` 做传输校验；安全收据始终使用 SHA-256，而非 MD5。

## 结果、失败与恢复

标准输出为 JSON，结果顺序与配置一致：

```json
{
  "sha256": "<trusted-ciphertext-sha256>",
  "bytes": 12345,
  "destinations": [
    {
      "destination_id": "r2",
      "state": "succeeded",
      "attempts": 1,
      "object_key": "lanqin/lanqin-<random>.age",
      "completed_at": "2026-10-29T00:00:00Z"
    },
    {
      "destination_id": "dav1",
      "state": "failed",
      "attempts": 1,
      "error_code": "access_denied"
    }
  ]
}
```

- 状态为 `succeeded`、`failed`、`cancelled` 或 `skipped`。只有成功结果包含最终 `object_key` 和 UTC `completed_at`；它们目前不写入应用数据库，不代表已完成持久化“最近成功”记录。
- 任一启用地点失败/取消，CLI 返回非零退出码，同时仍输出其他地点的结果。配置、收据或本地暂存预检失败时不发起上传，也不输出成功报告。全部停用时直接报错。
- 成功表示远端确认 PUT 完成（S3 为 200，WebDAV 为 200/201/204），**尚未进行下载回读、解密或恢复演练**；S3 的 202 异步接受不能当成完成。原始服务端错误正文、认证头和 SDK 错误不会回显，只输出稳定分类码。
- 网络断开、超时或取消时，远端可能已经接受对象；重试使用新名字，可能留下未记录的完整对象或供应商侧临时内容。当前不会自动删除这些对象，不能把云端所有 `.age` 文件都当成已确认备份，也不能据此执行保留清理。
- 将原始可信收据及成功报告在云端之外独立可信地保存。恢复时由操作者下载报告中该地点的准确对象，再向本地 `restore` 提供原始可信 SHA-256；从同一不可信云端取得的文件加摘要不构成来源认证。签名目录、远端浏览和自动恢复接入仍待实现。

## 网络与验证边界

只允许 HTTPS，验证服务器证书；无禁用 TLS 校验或允许内网的运行开关。不读取 HTTP(S) 代理环境变量，不跟随重定向，凭据仅发给该地点配置对应的 endpoint / S3 bucket 主机。每次建连解析 DNS、拒绝任一私有或特殊用途答案，并直接连接已验证 IP，避免二次解析切换到内网。内网 MinIO、自签名证书及只能通过代理访问的服务目前不在支持范围，不要通过关闭校验绕过。

自动测试使用合成 age 密文和本机 TLS 测试服务，执行真实 SDK 签名请求及 WebDAV PUT，覆盖多个同类地点、独立凭据签名验证、失败隔离、独立读偏移、重试、取消、收据预检、DNS 和重定向保护。另有合成 SQLite / 邮件快照从导出、加密、上传到取回密文并验证暂存恢复的闭环测试。**这些是隔离协议测试，不是已连接 R2、B2、AWS 或具体 WebDAV 供应商的生产联调证明。**

2026-10-29 本地验证记录：`go vet ./...`、`go test ./... -count=1`、`go mod verify` 通过；新增存储和 CLI 测试重复运行 3 次通过；Linux amd64 / arm64、`CGO_ENABLED=0` 构建通过。Windows CLI 子进程演练验证 keygen / create / 五地点缺失凭据的独立失败报告 / restore，没有向云端发送请求。原始 `gofmt -l .` 仍列出 56 个既有 CRLF 文件，换行归一化后无真实格式差异；未格式化这些无关文件。未进行 Linux 运行恢复、真实云供应商、管理界面或跨数据库备份验证。

实现参考：[AWS SDK v2 端点配置](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html)、[S3 PutObject 条件写入](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html)、[WebDAV RFC 4918](https://www.rfc-editor.org/rfc/rfc4918)。
